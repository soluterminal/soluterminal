package main

// requireNormalBootEngine is a second boundary behind main's dedicated
// update-only boot. Constructors without an error return fail closed before
// touching state or starting work if that boot ever reaches a normal engine.
// Use the captured start marker: the boot scrubs SOLU_UPDATE_ONLY from the
// environment so it cannot leak to installer children.
func requireNormalBootEngine(engine string) {
	if updateOnlyDaemon() {
		panic(engine + ": " + updateOnlyRefusal)
	}
}
