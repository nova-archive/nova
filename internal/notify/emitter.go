package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Destination is a resolved webhook target: the URL, an event-type filter (empty
// ⇒ all events), and an optional HMAC secret (nil ⇒ unsigned). cmd/coordinator
// resolves secret_file paths into Secret so this package stays config-free.
type Destination struct {
	URL    string
	Events []string
	Secret []byte
}

func (d Destination) accepts(eventType string) bool {
	if len(d.Events) == 0 {
		return true
	}
	for _, e := range d.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

// Options tunes a BestEffortHTTP emitter.
type Options struct {
	Timeout            time.Duration
	Concurrency        int
	Paranoid           bool
	MassCasualtyWindow time.Duration
	Now                func() time.Time
}

// BestEffortHTTP is a best-effort, optionally-HMAC-signed webhook Notifier
// (D-M5-9): no durable outbox, no retry. Deliveries run through a bounded worker
// semaphore so an event storm cannot fan out unbounded goroutines; an event that
// cannot acquire a slot is dropped with a warn. It honors paranoid gating and
// never panics into the caller's healing loop (recover at the goroutine boundary).
type BestEffortHTTP struct {
	dests              []Destination
	suppress           Suppressor
	client             *http.Client
	sem                chan struct{}
	paranoid           bool
	massCasualtyWindow time.Duration
	now                func() time.Time
}

// NewBestEffortHTTP wires an emitter. A nil suppress fires every event.
func NewBestEffortHTTP(dests []Destination, suppress Suppressor, o Options) *BestEffortHTTP {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.MassCasualtyWindow <= 0 {
		o.MassCasualtyWindow = time.Hour
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &BestEffortHTTP{
		dests:              dests,
		suppress:           suppress,
		client:             &http.Client{Timeout: o.Timeout},
		sem:                make(chan struct{}, o.Concurrency),
		paranoid:           o.Paranoid,
		massCasualtyWindow: o.MassCasualtyWindow,
		now:                now,
	}
}

// windowSeconds is the once-per-window suppression window per event type (D-M5-9a):
// shrinking 24h; node_revoked 0 (always, deduped upstream); the rest (degraded,
// concentrated, homogeneous) use the mass-casualty window.
func (e *BestEffortHTTP) windowSeconds(eventType string) int {
	switch eventType {
	case "federation.shrinking":
		return int((24 * time.Hour).Seconds())
	case "federation.node_revoked":
		return 0
	default:
		return int(e.massCasualtyWindow.Seconds())
	}
}

// Emit delivers ev to every matching destination, bounded + best-effort. The
// caller path is not blocked on network I/O (each delivery runs in a bounded
// goroutine), and paranoid gating skips emission entirely.
func (e *BestEffortHTTP) Emit(ctx context.Context, ev Event) {
	if e.paranoid {
		return
	}
	ts := e.now().Unix()
	body, err := json.Marshal(webhookBody{Type: ev.Type, ScopeKey: ev.ScopeKey, Payload: ev.Payload, Timestamp: ts})
	if err != nil {
		slog.Warn("nova.webhook.marshal_failed", "type", ev.Type, "err", err)
		return
	}
	for _, d := range e.dests {
		if !d.accepts(ev.Type) {
			continue
		}
		select {
		case e.sem <- struct{}{}:
			go e.deliver(ctx, d, ev, body, ts)
		default:
			slog.Warn("nova.webhook.dropped_saturated", "type", ev.Type, "dest", d.URL,
				"nova_webhook_delivery_failures_total", 1)
		}
	}
}

func (e *BestEffortHTTP) deliver(ctx context.Context, d Destination, ev Event, body []byte, ts int64) {
	defer func() {
		<-e.sem
		if r := recover(); r != nil {
			slog.Warn("nova.webhook.panic_recovered", "type", ev.Type, "dest", d.URL, "recover", r)
		}
	}()

	if e.suppress != nil {
		fired, err := e.suppress.TryFire(ctx, ev.Type, d.URL, ev.ScopeKey, e.windowSeconds(ev.Type))
		if err != nil {
			slog.Warn("nova.webhook.suppression_error", "type", ev.Type, "dest", d.URL, "err", err)
			return
		}
		if !fired {
			return // suppressed this window
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("nova.webhook.request_failed", "dest", d.URL, "err", err, "nova_webhook_delivery_failures_total", 1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if len(d.Secret) > 0 {
		signed := strconv.FormatInt(ts, 10) + "." + string(body)
		mac := hmac.New(sha256.New, d.Secret)
		mac.Write([]byte(signed))
		req.Header.Set("X-Nova-Webhook-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Nova-Webhook-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		slog.Warn("nova.webhook.delivery_failed", "dest", d.URL, "type", ev.Type, "err", err,
			"nova_webhook_delivery_failures_total", 1)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("nova.webhook.non_2xx", "dest", d.URL, "type", ev.Type, "status", resp.StatusCode,
			"nova_webhook_delivery_failures_total", 1)
		return
	}
	slog.Info("nova.webhook.delivered", "dest", d.URL, "type", ev.Type)
}

type webhookBody struct {
	Type      string         `json:"type"`
	ScopeKey  string         `json:"scope_key,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	Timestamp int64          `json:"timestamp"`
}

var _ Notifier = (*BestEffortHTTP)(nil)
