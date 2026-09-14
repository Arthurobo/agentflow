package uploads

// MigrateLegacyDir moves a leftover ~/.agentflow/uploads directory to Root().
// The daemon calls it once at startup, before anything writes to Root().
func MigrateLegacyDir(logf func(string, ...any)) {
	migrateLegacyUploadsDir(logf)
}
