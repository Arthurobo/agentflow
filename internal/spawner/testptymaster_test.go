package spawner

import "os"

// testPTYMaster adapts a raw *os.File to the ptyMasterFile interface for tests,
// with no-op resize/size. It is platform-neutral (no creack/pty dependency) so
// the tests build on every OS.
type testPTYMaster struct{ *os.File }

func (testPTYMaster) resize(uint16, uint16) error   { return nil }
func (testPTYMaster) size() (uint16, uint16, error) { return 0, 0, nil }
