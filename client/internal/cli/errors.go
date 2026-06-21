package cli

// cliError carries an exit code out of a cobra RunE. A silent error prints
// nothing (used for a remote command's own non-zero exit, where the stream has
// already produced all relevant output). A non-silent error prints its message
// to stderr. Transport/agent failures use session.TransportFailure (125) so
// scripts can distinguish them from a remote exit code (REQUIREMENTS §6).
type cliError struct {
	code   int
	err    error
	silent bool
}

func (e *cliError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return ""
}

func (e *cliError) Unwrap() error { return e.err }

// usageExitCode is returned for flag/usage errors, mirroring conventional tools.
const usageExitCode = 2
