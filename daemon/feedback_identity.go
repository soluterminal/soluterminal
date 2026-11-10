package main

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/feedbackredact"
)

var cachedFeedbackRedactor = sync.OnceValue(func() *feedbackredact.Redactor {
	return feedbackredact.New(feedbackLocalIdentity(os.Getenv, os.Hostname, os.UserHomeDir, user.Current, feedbackWindowsIdentity))
})

// Discovery is performed once, without logging any values. USERNAME and the
// profile variables cover native Windows; WSL may not inherit those variables,
// so a bounded, fixed command also reads the host's user and computer names.
func feedbackLocalIdentity(getenv func(string) string, hostname func() (string, error), home func() (string, error), current func() (*user.User, error), windows func() (string, string)) feedbackredact.Identity {
	var id feedbackredact.Identity
	if host, err := hostname(); err == nil {
		id.Hostnames = append(id.Hostnames, host)
	}
	id.Hostnames = append(id.Hostnames, getenv("COMPUTERNAME"))
	for _, key := range []string{"USER", "LOGNAME", "USERNAME"} {
		id.Usernames = append(id.Usernames, feedbackUserBase(getenv(key)))
	}
	if u, err := current(); err == nil {
		id.Usernames = append(id.Usernames, feedbackUserBase(u.Username))
	}
	if dir, err := home(); err == nil {
		id.Usernames = append(id.Usernames, feedbackUserBase(dir))
	}
	for _, key := range []string{"USERPROFILE", "HOMEPATH"} {
		id.Usernames = append(id.Usernames, feedbackUserBase(getenv(key)))
	}
	// In WSL, inherited PATH/APPDATA often identifies a different Windows user
	// even when interop is unavailable. These hints remain local to this cache.
	for _, key := range []string{"PATH", "APPDATA", "LOCALAPPDATA"} {
		for _, m := range feedbackWindowsProfile.FindAllStringSubmatch(getenv(key), -1) {
			id.Usernames = append(id.Usernames, m[1])
		}
	}
	winUser, winHost := windows()
	id.Usernames = append(id.Usernames, winUser)
	id.Hostnames = append(id.Hostnames, winHost)
	return id
}

var feedbackWindowsProfile = regexp.MustCompile(`(?i)(?:/mnt/[a-z]/Users/|[A-Z]:[\\/]Users[\\/])([^/\\:;]+)`)

func feedbackUserBase(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(s), `\`, "/"), "/")
	if s == "" {
		return ""
	}
	return filepath.Base(s)
}

func feedbackWindowsIdentity() (string, string) {
	if runtime.GOOS != "linux" {
		return "", ""
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || !strings.Contains(strings.ToLower(string(kernel)), "microsoft") {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/mnt/c/Windows/System32/cmd.exe", "/d", "/c", "set USERNAME&set COMPUTERNAME")
	applyNoWindow(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	b, err := cmd.Output()
	if err != nil || len(b) > 1024 {
		return "", ""
	}
	var username, hostname string
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "USERNAME":
			username = value
		case "COMPUTERNAME":
			hostname = value
		}
	}
	return username, hostname
}
