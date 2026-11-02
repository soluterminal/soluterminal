//! WSL address selection and eth0 discovery shared with the Windows proxy.

pub(crate) const FUNCTIONS: &str = include_str!("wsl_network.sh");

#[cfg_attr(not(windows), allow(dead_code))]
pub(crate) fn host_probe_script() -> String {
    // Discovery may probe eth0 in any mode to reuse daemons from older launchers.
    // Only wsl_daemon_host decides where a NEW daemon may bind.
    format!("{FUNCTIONS}\nwsl_eth0_ipv4\n")
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::process::Command;

    fn probe(mode: &str, mode_status: i32, addresses: &str, ip_status: i32, call: &str) -> String {
        // Functions stand in for the two WSL commands, through the same timeout
        // call sites as production. Values travel as arguments, never shell code.
        let script = format!(
            r#"set -e
mode_output="$1"; mode_status="$2"; ip_output="$3"; ip_status="$4"
timeout() {{ shift; "$@"; }}
wslinfo() {{ [ "$*" = '--networking-mode' ] || exit 90; printf '%s' "$mode_output"; return "$mode_status"; }}
ip() {{ [ "$*" = '-4 -o addr show dev eth0 scope global' ] || exit 91; printf '%s' "$ip_output"; return "$ip_status"; }}
{FUNCTIONS}
{call}
"#
        );
        let output = Command::new("bash")
            .args([
                "-c",
                &script,
                "test",
                mode,
                &mode_status.to_string(),
                addresses,
                &ip_status.to_string(),
            ])
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8(output.stdout).unwrap().trim().to_string()
    }

    const ETH0: &str = "2: eth0    inet 172.29.132.65/20 brd 172.29.143.255 scope global eth0\n";

    #[test]
    fn detects_runtime_modes_and_rejects_unrecognized_output() {
        for (input, expected) in [
            ("nat\n", "nat"),
            ("NAT\r\n", "nat"),
            ("Mirrored\n", "mirrored"),
            ("", "unknown"),
            ("virtioproxy", "unknown"),
            ("nat\nmirrored", "unknown"),
            ("networkingMode=nat", "unknown"),
        ] {
            assert_eq!(probe(input, 0, ETH0, 0, "wsl_networking_mode"), expected);
        }
        // Includes command-not-found and timeout. Partial output is untrusted.
        for status in [1, 124, 127] {
            assert_eq!(
                probe("nat", status, ETH0, 0, "wsl_networking_mode"),
                "unknown"
            );
        }
    }

    #[test]
    fn nat_uses_eth0_and_other_modes_use_loopback() {
        assert_eq!(probe("nat", 0, ETH0, 0, "wsl_daemon_host"), "172.29.132.65");
        for mode in [
            "mirrored",
            "bridged",
            "virtioproxy",
            "consomme",
            "none",
            "",
            "future-mode",
        ] {
            assert_eq!(probe(mode, 0, ETH0, 0, "wsl_daemon_host"), "127.0.0.1");
        }
        assert_eq!(probe("nat", 124, ETH0, 0, "wsl_daemon_host"), "127.0.0.1");
    }

    #[test]
    fn missing_or_invalid_eth0_falls_back_to_loopback() {
        for addr in [
            "",
            "junk",
            "0.0.0.0",
            "127.0.0.1",
            "169.254.1.2",
            "224.0.0.1",
            "255.255.255.255",
            "172.29.999.1",
            "172.29.1",
            "::1",
            "172.29.1.2;echo",
        ] {
            let line = format!("2: eth0 inet {addr}/20 scope global eth0\n");
            assert_eq!(
                probe("nat", 0, &line, 0, "wsl_daemon_host"),
                "127.0.0.1",
                "{addr}"
            );
        }
        assert_eq!(probe("nat", 0, "", 127, "wsl_daemon_host"), "127.0.0.1");
        assert_eq!(probe("nat", 0, ETH0, 124, "wsl_daemon_host"), "127.0.0.1");
    }

    #[test]
    fn discovery_can_still_reuse_an_eth0_daemon_when_mode_detection_fails() {
        assert_eq!(probe("", 127, ETH0, 0, "wsl_eth0_ipv4"), "172.29.132.65");
        assert!(host_probe_script().ends_with("\nwsl_eth0_ipv4\n"));
    }
}
