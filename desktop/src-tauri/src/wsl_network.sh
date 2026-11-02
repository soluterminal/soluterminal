# Shared by start-wsl.sh and the Windows upstream lookup. Query the running VM:
# .wslconfig can describe a pending change or a mode WSL failed to activate.
wsl_networking_mode() {
  local mode
  mode="$(timeout 2 wslinfo --networking-mode 2>/dev/null)" || mode=unknown
  mode="$(printf '%s' "$mode" | tr '[:upper:]' '[:lower:]' | tr -d '\r')"
  case "$mode" in
    nat|mirrored) printf '%s\n' "$mode" ;;
    *) printf 'unknown\n' ;;
  esac
}

wsl_eth0_ipv4() {
  local addresses
  # Only eth0, never hostname -I's Docker/VPN/other interface addresses.
  addresses="$(timeout 2 ip -4 -o addr show dev eth0 scope global 2>/dev/null)" || return 0
  printf '%s\n' "$addresses" | awk '
      $3 == "inet" {
        split($4, cidr, "/")
        n = split(cidr[1], octet, ".")
        valid = (n == 4)
        for (i = 1; i <= n; i++)
          if (octet[i] !~ /^[0-9]+$/ || octet[i] + 0 > 255) valid = 0
        if (valid && octet[1] + 0 > 0 && octet[1] + 0 < 224 &&
            octet[1] + 0 != 127 && !(octet[1] + 0 == 169 && octet[2] + 0 == 254)) {
          print cidr[1]; exit
        }
      }'
}

wsl_daemon_host() {
  local mode ip
  mode="$(wsl_networking_mode)"
  if [ "$mode" = nat ]; then
    ip="$(wsl_eth0_ipv4)" || ip=
    if [ -n "$ip" ]; then printf '%s\n' "$ip"; return; fi
  fi
  # Mirrored, unsupported/unknown mode, missing tools, or no usable eth0.
  printf '127.0.0.1\n'
}
