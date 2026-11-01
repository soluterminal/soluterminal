package main

// Optional extensions install notifier wiring at initialization.
// Builds without an extension leave this hook inert.
var bindAccountSwitchNotifiers = func(_ *uiBus, _ *apiServer, _, _ any) {}
