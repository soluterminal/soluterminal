package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	hostStorageOK             = "ok"
	hostStorageVHDXUnresolved = "vhdx_unresolved"
	hostStorageNotWSL         = "not_wsl"
	hostStorageDisabled       = "disabled"
	hostStorageNoPowerShell   = "no_powershell"
	hostStorageInterop        = "interop_unavailable"
	hostStorageUnsampled      = "unsampled"
	hostStorageTimeout        = "timeout"
	hostStorageParse          = "parse_error"
)

type hostStorageSample struct {
	At                time.Time `json:"at"`
	Available         bool      `json:"available"`
	State             string    `json:"state"`
	Detail            string    `json:"detail,omitempty"`
	Distro            string    `json:"distro,omitempty"`
	Volume            string    `json:"volume,omitempty"`
	FreeBytes         int64     `json:"freeBytes,omitempty"`
	TotalBytes        int64     `json:"totalBytes,omitempty"`
	VHDXPath          string    `json:"vhdxPath,omitempty"`
	VHDXPathRaw       string    `json:"-"`
	VHDXLogicalBytes  int64     `json:"vhdxLogicalBytes,omitempty"`
	VHDXPhysicalBytes int64     `json:"vhdxPhysicalBytes,omitempty"`
	VHDXSparse        *bool     `json:"sparse,omitempty"`
	LocalAppData      string    `json:"localAppData,omitempty"`
	OwnerSID          string    `json:"ownerSid,omitempty"`
	BasePath          string    `json:"-"`
	RegistrationCount int       `json:"registrationCount,omitempty"`
	ReparsePoint      bool      `json:"reparsePoint,omitempty"`
	DurationMS        int64     `json:"durationMs"`
	Source            string    `json:"source"`
}

type hostStorageSource interface {
	Sample(context.Context) hostStorageSample
	Latest() (hostStorageSample, bool)
	Refresh(context.Context) hostStorageSample
}

type hostStorageProber struct {
	insideWSL            func() bool
	distroName           func(context.Context) (string, error)
	resolvePS            func() (string, error)
	env                  func(string) []string
	run                  func(context.Context, string, []string, string) ([]byte, error)
	now                  func() time.Time
	timeout, minInterval time.Duration
	failBackoff          time.Duration

	mu           sync.Mutex
	latest       hostStorageSample
	hasLatest    bool
	failures     int
	backoffUntil time.Time
	lastRefresh  time.Time
}

var safeDistroName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func newWfHostStorageProber() *hostStorageProber {
	p := &hostStorageProber{insideWSL: isInsideWSL, resolvePS: resolvePowerShell, env: wslInteropEnv, now: time.Now, timeout: 12 * time.Second, minInterval: time.Minute, failBackoff: time.Minute}
	p.distroName = resolveDistroName
	p.run = func(ctx context.Context, psExe string, env []string, script string) ([]byte, error) {
		cmd := hiddenCommand(ctx, psExe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
		cmd.Env = env
		return cmd.Output()
	}
	return p
}

func resolveDistroName(ctx context.Context) (string, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := hiddenCommand(c, "wslpath", "-w", "/").Output()
	if err == nil {
		if name, ok := distroFromWindowsRoot(strings.TrimSpace(string(out))); ok {
			return name, nil
		}
	}
	if name := strings.TrimSpace(os.Getenv("WSL_DISTRO_NAME")); name != "" {
		return name, nil
	}
	return "", errors.New("WSL distro name unavailable")
}

func distroFromWindowsRoot(root string) (string, bool) {
	normal := strings.ReplaceAll(root, "/", `\`)
	lower := strings.ToLower(normal)
	for _, prefix := range []string{`\\wsl.localhost\`, `\\wsl$\`} {
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			rest := strings.TrimPrefix(normal, normal[:len(prefix)])
			name := strings.Split(rest, `\`)[0]
			return name, name != ""
		}
	}
	return "", false
}

func (p *hostStorageProber) Latest() (hostStorageSample, bool) {
	if p == nil {
		return hostStorageSample{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest, p.hasLatest
}

func (p *hostStorageProber) Sample(ctx context.Context) hostStorageSample {
	if p == nil {
		return hostStorageSample{At: time.Now().UTC(), State: hostStorageDisabled, Source: "live"}
	}
	now := p.now().UTC()
	p.mu.Lock()
	if p.hasLatest && (now.Sub(p.latest.At) < p.minInterval || now.Before(p.backoffUntil)) {
		s := p.latest
		s.Source = "cache"
		p.mu.Unlock()
		return s
	}
	p.mu.Unlock()
	return p.probe(ctx, false)
}

func (p *hostStorageProber) Refresh(ctx context.Context) hostStorageSample {
	if p == nil {
		return hostStorageSample{At: time.Now().UTC(), State: hostStorageDisabled, Source: "live"}
	}
	now := p.now().UTC()
	p.mu.Lock()
	if p.hasLatest && !p.lastRefresh.IsZero() && now.Sub(p.lastRefresh) < 30*time.Second {
		s := p.latest
		s.Source = "cache"
		s.Detail = "refresh rate limited"
		p.mu.Unlock()
		return s
	}
	p.lastRefresh = now
	if p.hasLatest && now.Before(p.backoffUntil) {
		s := p.latest
		s.Source = "cache"
		p.mu.Unlock()
		return s
	}
	p.mu.Unlock()
	return p.probe(ctx, true)
}

func (p *hostStorageProber) probe(parent context.Context, _ bool) hostStorageSample {
	started := p.now().UTC()
	finish := func(s hostStorageSample) hostStorageSample {
		s.At, s.DurationMS, s.Source = started, p.now().Sub(started).Milliseconds(), "live"
		raw := s.VHDXPathRaw
		if raw == "" {
			raw = s.VHDXPath
		}
		s.VHDXPath = redactSensitive(raw)
		s.Detail = redactSensitive(boundDetail(s.Detail))
		// VHDXPathRaw exists solely for the direct, live CLI result. Cached
		// samples can flow to APIs and telemetry, so never retain the raw user
		// path there.
		cached := s
		cached.VHDXPathRaw = ""
		p.mu.Lock()
		p.latest, p.hasLatest = cached, true
		if s.Available {
			p.failures = 0
			p.backoffUntil = time.Time{}
		} else {
			p.failures++
			shift := min(p.failures-1, 5)
			initial := p.failBackoff
			if initial <= 0 {
				initial = time.Minute
			}
			backoff := initial * time.Duration(1<<shift)
			if backoff > 30*time.Minute {
				backoff = 30 * time.Minute
			}
			p.backoffUntil = started.Add(backoff)
		}
		p.mu.Unlock()
		return s
	}
	if os.Getenv("SOLU_HOST_STORAGE_PROBE") == "0" {
		return finish(hostStorageSample{State: hostStorageDisabled})
	}
	if p.insideWSL == nil || !p.insideWSL() {
		return finish(hostStorageSample{State: hostStorageNotWSL})
	}

	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	type result struct{ s hostStorageSample }
	done := make(chan result, 1)
	go func() {
		name, err := p.distroName(ctx)
		if err != nil {
			done <- result{hostStorageSample{State: hostStorageParse, Detail: err.Error()}}
			return
		}
		if !safeDistroName.MatchString(name) {
			done <- result{hostStorageSample{State: hostStorageParse, Detail: "distro name unsafe"}}
			return
		}
		ps, err := p.resolvePS()
		if err != nil {
			done <- result{hostStorageSample{State: hostStorageNoPowerShell, Distro: name, Detail: err.Error()}}
			return
		}
		env := p.env(ps)
		out, err := p.run(ctx, ps, env, hostStorageScript(name))
		if err != nil {
			state := hostStorageParse
			low := strings.ToLower(err.Error())
			if strings.Contains(low, "accept4") || strings.Contains(low, "wsl_interop") || len(env) == 0 {
				state = hostStorageInterop
			}
			done <- result{hostStorageSample{State: state, Distro: name, Detail: err.Error()}}
			return
		}
		done <- result{parseHostStorageOutput(out, name)}
	}()
	select {
	case <-ctx.Done():
		// The production run seam uses exec.CommandContext, so cancellation
		// kills PowerShell. Give Output a bounded window to reap that child
		// before returning instead of accumulating timed-out probe goroutines.
		cancel()
		select {
		case <-done:
		case <-time.After(150 * time.Millisecond):
		}
		return finish(hostStorageSample{State: hostStorageTimeout, Detail: ctx.Err().Error()})
	case r := <-done:
		return finish(r.s)
	}
}

func boundDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

type hostStoragePSResult struct {
	OK                bool   `json:"ok"`
	Distro            string `json:"distro"`
	VHDX              string `json:"vhdx"`
	VHDXUnresolved    bool   `json:"vhdxUnresolved"`
	Volume            string `json:"volume"`
	FreeBytes         int64  `json:"freeBytes"`
	TotalBytes        int64  `json:"totalBytes"`
	VHDXLogicalBytes  int64  `json:"vhdxLogicalBytes"`
	VHDXPhysicalBytes int64  `json:"vhdxPhysicalBytes"`
	Sparse            *bool  `json:"sparse"`
	LocalAppData      string `json:"localAppData"`
	OwnerSID          string `json:"ownerSid"`
	BasePath          string `json:"basePath"`
	RegistrationCount int    `json:"registrationCount"`
	ReparsePoint      bool   `json:"reparsePoint"`
	Error             string `json:"error"`
}

func parseHostStorageOutput(out []byte, fallbackDistro string) hostStorageSample {
	last := ""
	scan := bufio.NewScanner(strings.NewReader(string(out)))
	for scan.Scan() {
		if line := strings.TrimSpace(scan.Text()); line != "" {
			last = line
		}
	}
	var r hostStoragePSResult
	if last == "" || json.Unmarshal([]byte(last), &r) != nil {
		return hostStorageSample{State: hostStorageParse, Distro: fallbackDistro, Detail: "invalid PowerShell output: " + boundDetail(last)}
	}
	state, available := hostStorageOK, true
	if r.VHDXUnresolved {
		state = hostStorageVHDXUnresolved
	}
	if !r.OK || r.FreeBytes < 0 || r.TotalBytes <= 0 {
		state, available = hostStorageParse, false
	}
	distro := r.Distro
	if distro == "" {
		distro = fallbackDistro
	}
	return hostStorageSample{Available: available, State: state, Detail: r.Error, Distro: distro, Volume: r.Volume, FreeBytes: r.FreeBytes, TotalBytes: r.TotalBytes, VHDXPath: r.VHDX, VHDXPathRaw: r.VHDX, VHDXLogicalBytes: r.VHDXLogicalBytes, VHDXPhysicalBytes: r.VHDXPhysicalBytes, VHDXSparse: r.Sparse, LocalAppData: r.LocalAppData, OwnerSID: r.OwnerSID, BasePath: r.BasePath, RegistrationCount: r.RegistrationCount, ReparsePoint: r.ReparsePoint}
}

func hostStorageScript(distro string) string {
	d := strings.ReplaceAll(distro, "'", "''")
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$distro='%s'
$result=[ordered]@{ok=$false;distro=$distro;vhdx='';vhdxUnresolved=$true;volume='';freeBytes=0;totalBytes=0;vhdxLogicalBytes=0;vhdxPhysicalBytes=0;sparse=$null;localAppData='';ownerSid='';basePath='';registrationCount=0;reparsePoint=$false;error=''}
try {
  $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
  if($null -eq $identity.User){throw 'Current Windows process SID is unavailable.'}
  $result.ownerSid=$identity.User.Value
  $local=[Environment]::ExpandEnvironmentVariables([string]$env:LOCALAPPDATA)
  if([string]::IsNullOrWhiteSpace($local) -or -not [IO.Path]::IsPathRooted($local)){throw 'LOCALAPPDATA is unavailable or not rooted.'}
  $local=[IO.Path]::GetFullPath($local).TrimEnd('\')
  $result.localAppData=$local
  $root=[IO.Path]::GetPathRoot($local)
  $hive='Registry::HKEY_USERS\'+$result.ownerSid+'\Software\Microsoft\Windows\CurrentVersion\Lxss'
  $registrations=@()
  if(Test-Path -LiteralPath $hive){$registrations=@(Get-ChildItem -LiteralPath $hive | ForEach-Object {Get-ItemProperty -LiteralPath $_.PSPath} | Where-Object {[string]::Equals([string]$_.DistributionName,$distro,[StringComparison]::OrdinalIgnoreCase)})}
  $result.registrationCount=$registrations.Count
  if($registrations.Count -eq 1){
    $raw=[Environment]::ExpandEnvironmentVariables([string]$registrations[0].BasePath)
    if([string]::IsNullOrWhiteSpace($raw)){throw 'Registered BasePath is empty.'}
    if($raw.StartsWith('\\?\')){$raw=$raw.Substring(4)}
    if($raw.StartsWith('\\') -or $raw.StartsWith('\\.\') -or -not [IO.Path]::IsPathRooted($raw)){throw 'Registered BasePath is not a safe rooted local path.'}
    $base=[IO.Path]::GetFullPath($raw).TrimEnd('\')
    $vhdx=[IO.Path]::GetFullPath((Join-Path $base 'ext4.vhdx'))
    $result.basePath=$base
    $result.vhdx=$vhdx
    $root=[IO.Path]::GetPathRoot($vhdx)
    $item=Get-Item -LiteralPath $vhdx -Force
    $identityPaths=@($local,$base,$vhdx)
    $prefix=$local.TrimEnd('\')+'\'
    if($base.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase)){
      $cursor=$local.TrimEnd('\')
      foreach($component in $base.Substring($prefix.Length).Split('\')){$cursor=Join-Path $cursor $component;$identityPaths+=$cursor}
    }
    foreach($identityPath in @($identityPaths|Select-Object -Unique)){
      $identityItem=Get-Item -LiteralPath $identityPath -Force
      if($identityItem.Attributes -band [IO.FileAttributes]::ReparsePoint){$result.reparsePoint=$true}
    }
    $result.vhdxLogicalBytes=[int64]$item.Length
    $result.sparse=[bool]($item.Attributes -band [IO.FileAttributes]::SparseFile)
    Add-Type -TypeDefinition 'using System;using System.Runtime.InteropServices;public static class SoluFileSize{[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern uint GetCompressedFileSizeW(string n,out uint h);}' -ErrorAction SilentlyContinue
    [uint32]$high=0
    [uint32]$low=[SoluFileSize]::GetCompressedFileSizeW($vhdx,[ref]$high)
    if($low -eq [uint32]::MaxValue -and [Runtime.InteropServices.Marshal]::GetLastWin32Error() -ne 0){throw 'Physical allocation measurement failed.'}
    $result.vhdxPhysicalBytes=([int64]$high -shl 32)-bor $low
    $result.vhdxUnresolved=$false
  } elseif($registrations.Count -eq 0) {
    $result.error='No current-owner registration matched the distro.'
  } else {
    $result.error='Multiple current-owner registrations matched the distro.'
  }
  $drive=[IO.DriveInfo]::new($root)
  $result.volume=$root
  $result.freeBytes=[int64]$drive.AvailableFreeSpace
  $result.totalBytes=[int64]$drive.TotalSize
  $result.ok=$true
} catch {$result.error=$_.Exception.Message}
$result|ConvertTo-Json -Compress`, d)
}
