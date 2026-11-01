package main

import "strings"

// Bash imports exported functions before reading ~/.bashrc. Giving terminal
// panes this function makes `codex` typed at a Bash prompt use the same inline
// mode as a Codex pane launched by SoluTerminal. A user's own shell function in
// ~/.bashrc still takes precedence. Inline mode is interactive-shell-only;
// approval isolation also covers the noninteractive managed login wrapper.
const codexInlineBashEnv = "BASH_FUNC_codex%%=" + codexPaneBashFunction

// Only fixed categories and a validated HTTP status may reach the terminal.
// Exceptions can contain daemon addresses, response bodies or credentials;
// never render their Message/ToString or traverse an unbounded inner chain.
// Release the capability minted for a terminal hook once the CLI exits.
// $managedEnv is filled only for a managed launch, so a default launch
// passes no token. Best effort and silent: a lost release
// is reclaimed when the daemon's store is full.
const launchRoutePowerShellRelease = `{ param($daemon, [string]$token)
  if (-not $daemon -or $token -cnotmatch '^[0-9a-f]{64}$') { return }
  try {
    $release = [Net.HttpWebRequest]::Create(([Uri]$daemon).AbsoluteUri.TrimEnd('/') + '/api/team-broker/capability/release')
    $release.Method = 'POST'; $release.ContentType = 'application/json'; $release.Proxy = $null
    $release.AllowAutoRedirect = $false; $release.Timeout = 5000; $release.ReadWriteTimeout = 5000
    $releaseBody = [Text.Encoding]::UTF8.GetBytes('{"token":"' + $token + '"}'); $release.ContentLength = $releaseBody.Length
    $releaseStream = $release.GetRequestStream()
    try { $releaseStream.Write($releaseBody,0,$releaseBody.Length) } finally { $releaseStream.Dispose() }
    $release.GetResponse().Dispose()
  } catch { }
}`

const codexPowerShellRoutingDiagnostic = `{ param([Exception]$exception, [string]$phase)
  if (@('invalid_address','response_too_large','invalid_json','invalid_mode','invalid_team_plan') -ccontains $phase) { return $phase }
  for ($depth = 0; $null -ne $exception -and $depth -lt 4; $depth++) {
    if ($exception -is [Net.WebException]) {
      switch -CaseSensitive ([string]$exception.Status) {
        'ProtocolError' {
          if ($exception.Response -is [Net.HttpWebResponse]) {
            $status = [int]$exception.Response.StatusCode
            if ($status -ge 100 -and $status -le 599) { return ('http_' + $status) }
          }
          return 'http_error'
        }
        'Timeout' { return 'network_timeout' }
        'ConnectFailure' { return 'network_connect' }
        'NameResolutionFailure' { return 'network_dns' }
        'ProxyNameResolutionFailure' { return 'network_dns' }
        'TrustFailure' { return 'network_tls' }
        'SecureChannelFailure' { return 'network_tls' }
        'ReceiveFailure' { return 'network_receive' }
        'SendFailure' { return 'network_send' }
        'ConnectionClosed' { return 'network_closed' }
        'KeepAliveFailure' { return 'network_closed' }
        'RequestCanceled' { return 'network_cancelled' }
        default { return 'network_error' }
      }
    }
    $exception = $exception.InnerException
  }
  return 'request_failed'
}`

// PowerShell reads its normal profile before -Command. Install the function
// only if that profile did not already define a codex alias or function, then
// keep the shell open at its ordinary prompt. Resolve an external Codex command
// inside each invocation so a later CLI install or PATH change still works.
const codexInlinePowerShellInit = `if (-not (Get-Command codex -CommandType Alias,Function -ErrorAction SilentlyContinue)) {
  function global:codex {
    $target = Get-Command codex -CommandType Application,ExternalScript -ErrorAction Stop | Select-Object -First 1
    $skipValue = $false; $sawPositional = $false; $interactive = $true; $modelInvocation = $true
    $needInline = $true; $needDaemon = $true
    foreach ($arg in $args) {
      if ($skipValue) { $skipValue = $false; continue }
      if ($arg -ceq '--') { break }
      if ($arg -ceq '--no-alt-screen') { $needInline = $false; continue }
      if ($arg -ceq '--no-daemon') { $needDaemon = $false; continue }
      if (@('--remote','--help','-h','--version','-V') -ccontains $arg -or $arg -clike '--remote=*') { $interactive = $false; $modelInvocation = $false; break }
      if (@('-c','--config','--enable','--disable','--remote-auth-token-env','-i','--image','-m','--model','--local-provider','-p','--profile','-s','--sandbox','-C','--cd','--add-dir','-a','--ask-for-approval') -ccontains $arg) { $skipValue = $true; continue }
      if ($arg.StartsWith('-') -or $sawPositional) { continue }
      $sawPositional = $true
      if (@('exec','e','review','apply','a','agents','login','logout','mcp','plugin','app-server','remote-control','completion','update','doctor','sandbox','debug','queue','archive','delete','migrate-rollouts','unarchive','cloud','exec-server','features','help') -ccontains $arg) { $interactive = $false; $modelInvocation = @('exec','e','review','app-server') -ccontains $arg }
    }
    $extra = @()
    if ($interactive) {
      if ($needDaemon) {
        $helpText = & $target.Definition --help 2>$null
        if ($LASTEXITCODE -eq 0 -and ($helpText -join ' ') -clike '*--no-daemon*') { $extra += '--no-daemon' }
      }
      if ($needInline) { $extra += '--no-alt-screen' }
    }
    $plan = $null; $userConfig = $false
    if ($modelInvocation -and $env:SOLU_TEAM_BROKER_DAEMON_URL) {
      # The fetch scriptblock has a child scope; this shared object preserves
      # only evidence established by explicit validation, never response text.
      $routingEvidence = @{ phase = 'invalid_address' }
      try {
        $daemon = [Uri]$env:SOLU_TEAM_BROKER_DAEMON_URL
        if ($daemon.Scheme -cne 'http' -or @('127.0.0.1','[::1]','::1') -cnotcontains $daemon.Host -or $daemon.UserInfo -or $daemon.Query -or $daemon.Fragment -or $daemon.AbsolutePath -cne '/') { throw 'Invalid local daemon address' }
        $routingEvidence.phase = 'request_failed'
        $fetchPlan = {
          param([string]$query)
          $routingEvidence.phase = 'request_failed'
          $request = [Net.HttpWebRequest]::Create($daemon.AbsoluteUri.TrimEnd('/') + '/api/team-broker/codex-shell-plan' + $query)
          $request.Method = 'POST'; $request.ContentType = 'application/json'; $request.Proxy = $null
          $request.AllowAutoRedirect = $false; $request.Timeout = 15000; $request.ReadWriteTimeout = 15000
          $body = [Text.Encoding]::UTF8.GetBytes('{}'); $request.ContentLength = $body.Length
          $stream = $request.GetRequestStream()
          try { $stream.Write($body,0,$body.Length) } finally { $stream.Dispose() }
          $response = $request.GetResponse()
          try {
            $reader = New-Object IO.StreamReader($response.GetResponseStream())
            try {
              $buffer = New-Object char[] 4096; $raw = New-Object Text.StringBuilder
              while (($count = $reader.Read($buffer,0,$buffer.Length)) -gt 0) {
                if ($raw.Length + $count -gt 65536) { $routingEvidence.phase = 'response_too_large'; throw 'Invalid launch plan' }
                [void]$raw.Append($buffer,0,$count)
              }
              $raw = $raw.ToString()
            } finally { $reader.Dispose() }
            try { $result = $raw | ConvertFrom-Json -ErrorAction Stop }
            catch { $routingEvidence.phase = 'invalid_json'; throw }
          } finally { $response.Dispose() }
          if (@('personal','team') -cnotcontains $result.mode) { $routingEvidence.phase = 'invalid_mode'; throw 'Invalid launch plan' }
          if ($result.mode -ceq 'team' -and (-not $result.environment -or -not $result.overrides)) { $routingEvidence.phase = 'invalid_team_plan'; throw 'Account route unavailable. Invalid launch plan.' }
          return $result
        }
        # Ask for the user's own CODEX_HOME first; personal routing costs nothing
        # more. Only Codex 0.93+ accepts the ephemeral credential store that keeps
        # the personal login out of a selected account, so an older Codex (or an
        # unreadable version) gets the isolated home instead.
        $plan = & $fetchPlan '?config=user'
        if ($plan.mode -ceq 'team' -and -not (@($plan.environment) -clike 'CODEX_HOME=*')) {
          # Windows PowerShell turns native stderr (Codex warnings) into errors
          # under a 'Stop' preference; any probe failure means "unsupported".
          $versionText = $null
          try { $versionText = & { $ErrorActionPreference = 'Continue'; & $target.Definition --version 2>$null } } catch { $versionText = $null }
          if (($versionText -join ' ') -match 'codex-cli\s+(\d+)\.(\d+)\.(\d+)') {
            $userConfig = [version]"$($Matches[1]).$($Matches[2]).$($Matches[3])" -ge [version]'0.93.0'
          }
          if (-not $userConfig) { $plan = & $fetchPlan '' }
        }
      } catch {
        $routingCode = & ` + codexPowerShellRoutingDiagnostic + ` $_.Exception $routingEvidence.phase
        $routingHint = Get-Variable -Name SoluCodexRoutingHint -Scope Global -ValueOnly -ErrorAction SilentlyContinue
        throw ("` + codexShellRoutingMessage + `" + $routingHint + ' [solu-routing:' + $routingCode + ']')
      }
    }
    $savedEnv = New-Object Collections.Hashtable ([StringComparer]::Ordinal)
    $managedEnv = New-Object Collections.Hashtable ([StringComparer]::Ordinal)
    if ($plan -and $plan.mode -ceq 'team') {
      for ($i = 0; $i -lt $args.Count; $i++) {
        $config = $null; $arg = [string]$args[$i]
        if ($arg -ceq '--') { break }
        if (@('-c','--config') -ccontains $arg -and $i + 1 -lt $args.Count) { $i++; $config = [string]$args[$i] }
        elseif ($arg.StartsWith('--config=')) { $config = $arg.Substring(9) }
        elseif ($arg.StartsWith('-c') -and $arg.Length -gt 2) { $config = $arg.Substring(2) }
        elseif (@('--enable','--disable','--remote-auth-token-env','-i','--image','-m','--model','-p','--profile','-s','--sandbox','-C','--cd','--add-dir','-a','--ask-for-approval','--output-last-message','-o','--output-schema') -ccontains $arg) { $i++; continue }
        if ($config) {
          $key = $config.Split('=')[0].Replace('"','').Replace("'",'').Trim()
          if ($key -match '^(model_provider$|model_providers[.\[]|openai_base_url$|chatgpt_base_url$|cli_auth_credentials_store$)') { throw 'Account route unavailable. Select a personal account before overriding Codex provider routing.' }
        }
        if ($arg -ceq '--oss' -or $arg -ceq '--local-provider' -or $arg.StartsWith('--local-provider=')) { throw 'Account route unavailable. Select a personal account before overriding Codex provider routing.' }
      }
      foreach ($entry in $plan.environment) {
        $entry = [string]$entry; $separator = $entry.IndexOf('=')
        if ($separator -le 0) { throw 'Account route unavailable. Invalid launch environment.' }
        $name = $entry.Substring(0,$separator); $value = $entry.Substring($separator+1)
        if (@('CODEX_HOME','SOLU_TEAM_BROKER_TOKEN','SOLU_TEAM_BROKER_PROVIDER') -cnotcontains $name -or -not $value) { throw 'Account route unavailable. Invalid launch environment.' }
        $managedEnv[$name] = $value
      }
      if (-not $managedEnv['SOLU_TEAM_BROKER_TOKEN'] -or $managedEnv['SOLU_TEAM_BROKER_PROVIDER'] -cne 'codex') { throw 'Account route unavailable. Incomplete launch environment.' }
      # Without an isolated CODEX_HOME the plan runs in the user's own home: only
      # for a Codex verified above, and only with the ephemeral credential store.
      if (-not $managedEnv['CODEX_HOME'] -and (-not $userConfig -or @($plan.overrides) -cnotcontains 'cli_auth_credentials_store="ephemeral"')) { throw 'Account route unavailable. Incomplete launch environment.' }
      # Only our loopback route bypasses a proxy. Preserve all existing
      # exclusions and HTTP(S)/ALL_PROXY settings for external traffic.
      $noProxy = (@($env:NO_PROXY,$env:no_proxy,'127.0.0.1','localhost','::1') | Where-Object { $_ }) -join ','
      $managedEnv['NO_PROXY'] = $noProxy; $managedEnv['no_proxy'] = $noProxy
      # The user's own CODEX_HOME stays unless an isolated plan replaces it.
      foreach ($name in @(@('OPENAI_API_KEY','CODEX_API_KEY','CODEX_ACCESS_TOKEN','OPENAI_BASE_URL','SOLU_TEAM_BROKER_TOKEN','SOLU_TEAM_BROKER_PROVIDER','NO_PROXY','no_proxy') + @($managedEnv.Keys))) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name,'Process') }
    }
    $forward = $extra + @($args)
    if ($plan -and $plan.mode -ceq 'team') {
      if ($interactive) {
        $forward = @($plan.overrides) + $forward
      } else {
        # Older Codex exec builds read subcommand config overrides, not the
        # parent's -c flags. Keep route flags in that parser's scope.
        $delimiter = -1
        for ($i = 0; $i -lt $forward.Count; $i++) {
          if ($forward[$i] -ceq '--') { $delimiter = $i; break }
          if (@('-c','--config','--enable','--disable','--remote-auth-token-env','-i','--image','-m','--model','-p','--profile','-s','--sandbox','-C','--cd','--add-dir','-a','--ask-for-approval','--output-last-message','-o','--output-schema') -ccontains $forward[$i]) { $i++ }
        }
        if ($delimiter -lt 0) { $forward = $forward + @($plan.overrides) }
        else {
          $before = @(); if ($delimiter -gt 0) { $before = @($forward[0..($delimiter-1)]) }
          $forward = $before + @($plan.overrides) + @($forward[$delimiter..($forward.Count-1)])
        }
      }
    }
    $childExitCode = $null
    try {
      if ($managedEnv.Count -gt 0) {
        foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name,$null,'Process') }
        foreach ($name in $managedEnv.Keys) { [Environment]::SetEnvironmentVariable($name,$managedEnv[$name],'Process') }
      }
      & $target.Definition @forward
      $childExitCode = $LASTEXITCODE
    } finally {
      foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name,$savedEnv[$name],'Process') }
      & ` + launchRoutePowerShellRelease + ` $daemon $managedEnv['SOLU_TEAM_BROKER_TOKEN']
    }
    if ($null -ne $childExitCode) { $global:LASTEXITCODE = $childExitCode }
  }
}`

func codexInlinePowerShellCmd(cmd []string) []string {
	if len(cmd) != 1 {
		return nil
	}
	name := strings.ToLower(strings.ReplaceAll(cmd[0], `\`, "/"))
	name = name[strings.LastIndexByte(name, '/')+1:]
	if name != "pwsh.exe" && name != "powershell.exe" && name != "pwsh" && name != "powershell" {
		return nil
	}
	return []string{cmd[0], "-NoExit", "-Command", paneShellPowerShellInit}
}

// Terminal PowerShell panes get the `codex` hook and every registered pane
// shell init (registerPaneShellInit).
var paneShellPowerShellInit = codexInlinePowerShellInit

// codexInlineTerminalCmd changes only the process argv. Pane.Cmd remains the
// original command so resume detection and persisted workspace definitions keep
// their existing shape. Codex's normal buffer lets SoluTerminal own scrollback
// and show its Jump to latest control.
func codexInlineTerminalCmd(cmd []string) []string {
	if !codexPaneNeedsOption(cmd, "--no-alt-screen") {
		return nil
	}
	out := make([]string, 0, len(cmd)+1)
	out = append(out, cmd[0], "--no-alt-screen")
	out = append(out, cmd[1:]...)
	return out
}
