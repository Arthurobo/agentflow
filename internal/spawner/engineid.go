package spawner

// resolveEngineID normalises a caller-supplied engine id to its canonical
// form, defaulting to "claude" when empty or unknown. The spawner does NOT
// import internal/engine (avoids a cycle; engine wraps the spawner's
// Options): the engine package is consulted by the agentd wiring layer, and
// the spawner only needs to know the string id.
func resolveEngineID(s string) string {
	switch s {
	case "opencode":
		return "opencode"
	default:
		return "claude"
	}
}
