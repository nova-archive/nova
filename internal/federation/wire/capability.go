package wire

// NegotiateCapabilities reports whether offered ⊇ required. When it is not, ok
// is false and missing lists the required capabilities the donor did not offer,
// in required's order. Negotiation FAILS CLOSED: an empty overlap is a clear
// failure, never a silent downgrade (D-cap, D7).
func NegotiateCapabilities(offered, required []string) (missing []string, ok bool) {
	have := make(map[string]struct{}, len(offered))
	for _, c := range offered {
		have[c] = struct{}{}
	}
	for _, r := range required {
		if _, found := have[r]; !found {
			missing = append(missing, r)
		}
	}
	return missing, len(missing) == 0
}
