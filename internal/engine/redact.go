package engine

import "github.com/PosvdM/magpie-lamplighter/internal/redact"

// The engine redacts (redact.Text) every error text where it leaves the
// engine or is kept on disk: log lines (logf, and the ignition record of
// executeStream), events with their history records (addAccountEvent),
// the targets' errors in state.json (package ignite), the credential
// problems' summaries, the status (redactStatus: the errors of the
// accounts, the account list, the model list, the configuration and the
// webhook settings, kept unredacted in memory), the history, and the
// answers of the refresh, test notification and MCP calls. Texts kept on
// disk before they were redacted are redacted where they are read:
// Status, HistoryRange and loadRecentEvents.

// errText is the text of err, redacted.
func errText(err error) string { return redact.Text(err.Error()) }

// redactEvent is ev with its message, detail and params redacted. Label
// is left as it is: the page matches events to accounts by it.
func redactEvent(ev Event) Event {
	ev.Message = redact.Text(ev.Message)
	ev.Detail = redact.Text(ev.Detail)
	ev.Params = redact.Params(ev.Params)
	return ev
}

// redactStatus redacts the error texts of s, a copy of the engine's
// state.
func redactStatus(s *Status) {
	s.ConfigError = redact.Text(s.ConfigError)
	s.ModelsError = redact.Text(s.ModelsError)
	s.ListError = redact.Text(s.ListError)
	s.NotifyError = redact.Text(s.NotifyError)
	for i := range s.Accounts {
		s.Accounts[i].Error = redact.Text(s.Accounts[i].Error)
	}
	for i := range s.Targets {
		s.Targets[i].LastError = redact.Text(s.Targets[i].LastError)
		s.Targets[i].CircuitReason = redact.Text(s.Targets[i].CircuitReason)
	}
	for i := range s.Events {
		s.Events[i] = redactEvent(s.Events[i])
	}
}
