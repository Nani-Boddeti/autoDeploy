package checkoutcontain

import (
	"errors"
	"io"
)

var (
	ErrSpawnDenied  = errors.New("checkout spawn service denied request")
	ErrSpawnInvalid = errors.New("checkout spawn service invalid request")
)

// GitOperation is a closed set translated by the root helper. It is never a
// caller supplied git command line.
type GitOperation uint8

const (
	GitInit GitOperation = iota + 1
	GitRemoteAddOrigin
	GitFetchExact
	GitRevParseFetchHead
	GitRevParseHead
	GitCheckoutFetchHead
	GitLSTree
	GitGrepLFS
)

type SpawnLimits struct{ TimeoutSeconds, MaxOutput uint32 }

// SpawnRequest transfers exactly WorkspaceFD, StdoutFD and StderrFD to the
// helper. For fetch it transfers AskpassReadFD as a fourth FD. Run consumes
// every supplied raw descriptor on every return path. StdoutReadFD and
// StderrReadFD are transferred to its drain wrappers after send.
type SpawnRequest struct {
	Operation                                 GitOperation
	Arg, RemoteURL                            string
	WorkspaceFD, StdoutFD, StderrFD           int
	StdoutReadFD, StderrReadFD, AskpassReadFD int
	Stdout, Stderr                            io.Writer
	Limits                                    SpawnLimits
}

type ExitClass uint8

const (
	ExitSuccess ExitClass = iota
	ExitFailed
	ExitTimedOut
	ExitIndeterminate
)

// SpawnResult intentionally excludes provider output; it is streamed only to
// the redacting writers passed in SpawnRequest.
type SpawnResult struct {
	ExitCode int
	Class    ExitClass
}

type SpawnClient struct {
	SocketPath string
	AgentGID   uint32
	// MaxOutput is the operator-configured combined stdout/stderr limit.
	MaxOutput uint32
}
