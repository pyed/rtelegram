package tailer

// FileTailer is a stubbed tailer that exposes empty channels.
type FileTailer struct {
	lines  chan string
	errors chan error
}

// RunFileTailer returns a tailer with closed channels.
func RunFileTailer(string, bool, interface{}) *FileTailer {
	lines := make(chan string)
	errors := make(chan error)
	close(lines)
	close(errors)
	return &FileTailer{lines: lines, errors: errors}
}

// Lines exposes the lines channel.
func (ft *FileTailer) Lines() <-chan string { return ft.lines }

// Errors exposes the errors channel.
func (ft *FileTailer) Errors() <-chan error { return ft.errors }
