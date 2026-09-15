- The spoke-side heartbeat-signature verifier now persists its
  trust-on-first-signed flag and rollback-seq floor to
  `/data/heartbeat-sig-state.json` (write-through on each accepted signed
  response, fail-open on missing/corrupt state), so a spoke restart can no
  longer be used to strip signatures back to the pre-trust accept path or to
  replay a previously captured signed response in enforce mode (#7121).
