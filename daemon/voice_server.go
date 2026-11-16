package main

import "net/http"

// voiceServerState is the API server's voice state: cleanup on the user's
// own Claude login (voice_local.go), local voice settings and the
// vocabulary. It lives in a slot (extensions.go) because the core server
// has no voice.
type voiceServerState struct {
	localCleaner *localCleaner
	voiceLocal   *voiceLocalSettings
	voiceWords   *voiceWords
}

var voiceServerSlot extSlot[voiceServerState]

func (a *apiServer) voice() *voiceServerState { return voiceServerSlot.ensure(&a.ext) }

func (a *apiServer) localCleaner() *localCleaner     { return a.voice().localCleaner }
func (a *apiServer) voiceLocal() *voiceLocalSettings { return a.voice().voiceLocal }
func (a *apiServer) voiceWords() *voiceWords         { return a.voice().voiceWords }

func init() {
	voiceBuiltIn = true
	registerBootStep(bootServer, func(b *bootEnv) {
		api := b.api
		api.voice().localCleaner = newLocalCleaner()
		api.voice().localCleaner.useAPIKey = func() bool { return api.localCleanupMode() == voiceCleanupAPIKey }
		api.voice().voiceLocal = newVoiceLocalSettings(b.dataDir)
		api.voice().voiceWords = newVoiceWords(b.dataDir)
	})
	registerAPIRoutes(func(a *apiServer, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/transcribe", a.transcribeAudio)
		mux.HandleFunc("POST /api/voice/cleanup", a.voiceCleanup)
		mux.HandleFunc("POST /api/voice/prewarm", a.handleVoicePrewarm)
		mux.HandleFunc("GET /api/voice/local", a.handleGetVoiceLocal)
		mux.HandleFunc("PUT /api/voice/local", a.handlePutVoiceLocal)
		// Words to recognise (voice_vocabulary.go). PUT is local-only.
		mux.HandleFunc("GET /api/voice/vocabulary", a.handleGetVoiceWords)
		mux.HandleFunc("PUT /api/voice/vocabulary", a.handlePutVoiceWords)
	})
}
