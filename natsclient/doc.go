// Package natsclient is the one NATS client every Go service on the platform
// uses:
//
//   - Connect dials with the projected token and the service's identity,
//     re-reads the token on every (re)connect, reconnects forever, logs
//     every connection event and gives the identity a private reply inbox
//     (_INBOX_<ns>_<sa>) so no other identity can read its replies;
//   - Request is a synchronous call with a deadline, W3C trace context in the
//     headers, and distinct errors for "nobody serves this subject"
//     (ErrNoResponders) and "the deadline passed" (ErrTimeout), counted and
//     timed as nats_requests_total and nats_request_duration_seconds;
//   - Service registers request/reply endpoints on <service>.svc.<endpoint>
//     through nats.go's micro package, in a queue group named after the
//     service by default, with the caller's trace continued in a server span.
//
// The subject shapes follow infrastructure ADR 0006: <service>.svc.<endpoint>
// for synchronous calls on core NATS, <service>.evt.<event> for JetStream
// facts and <service>.cmd.<command> for JetStream work. This package covers
// the synchronous half; events and commands are generated from the service's
// AsyncAPI contract.
package natsclient
