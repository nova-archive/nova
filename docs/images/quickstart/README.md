# Quickstart screenshots — capture checklist

`docs/quickstart.md` references five screenshots of the first-run setup
wizard. They have not been captured yet (the doc's image links 404 until
they land here). Capture them at the wizard (`http://127.0.0.1:8444/setup/`)
against a **disposable** instance — the master-key screenshot necessarily
shows key material, so the instance you photograph must never carry real
data (tear it down with `--profile setup down -v` afterwards).

Required captures (PNG, light theme, full wizard card in frame):

| File | What must be visible |
| --- | --- |
| `01-welcome.png` | The "Welcome to Nova" step (Step 2) with the hostname, contact-email, and display-name fields, plus the progress-dot rail. |
| `02-master-key.png` | The "Your master key" step (Step 3) with the hex key box, the fingerprint row, the **Download backup** button, and the typed-fingerprint confirm field with Next still disabled. |
| `03-tls-mode.png` | The "TLS & certificates" step (Step 6) with the mode dropdown open or set to `http-01` so the CT-log privacy warning text underneath is readable. |
| `04-review.png` | The "Review" step (Step 9) showing the full answers table (hostname, admin email, TLS mode, public uploads, paranoid) and the **Submit** button. |
| `05-live.png` | The "You're live" orientation step (Step 11) with the `/admin` link, the embed-snippet code block, and the master-key-backup reminder. |
