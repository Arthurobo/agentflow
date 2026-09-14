package eventmodel

import (
	"path/filepath"
)

// ProjectFromCWD derives the canonical `project` value from a working
// directory: a short repo label. The only universally available signal on raw
// records is `cwd`, so project is the basename of the working directory
// (e.g. /home/u/code/myproject -> "myproject").
func ProjectFromCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if cwd == "." || cwd == "/" || cwd == string(filepath.Separator) {
		return ""
	}
	return filepath.Base(cwd)
}
