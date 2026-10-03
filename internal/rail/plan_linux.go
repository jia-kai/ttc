package rail

import "io/fs"

// sandboxPlan is the planner's owned result, not a config or host snapshot.
// Paths are resolved in their respective namespaces; operations are already in
// execution order. Consumers must neither reorder operations nor inspect the
// filesystem to reinterpret them. Owned host observations support bounded
// pre-launch revalidation, not a durable snapshot or atomic check-to-mount guarantee.
type sandboxPlan struct {
	hostname, workdir            string
	namespaces                   []namespaceKind
	newSession, dropCapabilities bool
	filesystem                   []filesystemOperation
	environment                  []environmentChange
	tmuxConfig                   string            // Named sandbox entrypoint selected during host inspection.
	hostObservations             []hostObservation // Owned, sorted facts inspected during planning.
}

type namespaceKind uint8

const (
	namespaceUser namespaceKind = iota + 1
	namespaceProcess
	namespaceHostname
)

type filesystemKind uint8

const (
	filesystemBind filesystemKind = iota + 1
	filesystemSymlink
	filesystemProc
	filesystemDevices
	filesystemTmpfs
	filesystemDirectory
)

// filesystemOperation describes one ordered namespace mutation. source is an
// absolute canonical host path for binds, or a literal target for symlinks.
// dest is an absolute sandbox path. writable applies to binds and tmpfs only;
// mode is the creation permission bits for synthetic directories only.
type filesystemOperation struct {
	kind         filesystemKind
	source, dest string
	writable     bool
	mode         fs.FileMode
}

// An unset removes an inherited variable; an empty value alone does not unset it.
type environmentChange struct {
	name, value string
	unset       bool
}

// sandboxProcess supplies execution choices, not filesystem policy. command
// contains the executable and arguments inside the sandbox. environment is the
// captured host process environment. dieWithParent ties the sandbox lifetime to
// its detached supervisor, not to an attaching client.
type sandboxProcess struct {
	command       []string
	environment   []string
	dieWithParent bool
}

// invocation is a fully lowered OS command. Its slices are owned by the result;
// execution adds only context, streams and process/descriptor lifecycle handling.
type invocation struct {
	executable  string
	args        []string
	environment []string
}
