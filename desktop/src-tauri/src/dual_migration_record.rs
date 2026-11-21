//! Strict private recovery records. Never derive Debug for an intent: its
//! claim hash belongs only in the private durable file, never diagnostics/IPC.
//! Decoding alone grants no authority to mutate a claim or clear a fence.

use serde::{Deserialize, Serialize};
use std::{collections::HashSet, io};

const MAX_RECORD_BYTES: usize = 16 * 1024;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub(crate) struct Intent {
    schema: u32,
    migration_id: String,
    host_machine_id: String,
    wsl_machine_id: String,
    distro: String,
    linux_user: String,
    service_data_dir: String,
    default_data_dir: String,
    original_path: String,
    retired_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    claim_sha256: Option<String>,
}

fn invalid() -> io::Error {
    // No raw serde error, path, field contents or claim fingerprint escapes.
    io::Error::new(
        io::ErrorKind::InvalidData,
        "migration recovery record is invalid; migration interrupted",
    )
}

fn identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_:".contains(&c))
}

fn lowercase_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}

fn linux_path(value: &str) -> bool {
    value.starts_with('/')
        && value.len() <= 4096
        && !value.chars().any(char::is_control)
        && !value
            .split('/')
            .skip(1)
            .any(|part| part.is_empty() || part == "." || part == "..")
}

impl Intent {
    pub(crate) fn decode(bytes: &[u8]) -> io::Result<Self> {
        if bytes.len() > MAX_RECORD_BYTES {
            return Err(invalid());
        }
        let record: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        record.validate()?;
        Ok(record)
    }

    fn validate(&self) -> io::Result<()> {
        let retired_prefix = format!("{}.retired-", self.original_path);
        let valid = self.schema == 1
            && lowercase_hex(&self.migration_id, 32)
            && identifier(&self.host_machine_id)
            && identifier(&self.wsl_machine_id)
            && !self.distro.is_empty()
            && self.distro.len() <= 256
            && !self.distro.chars().any(char::is_control)
            && !self.linux_user.is_empty()
            && self.linux_user.len() <= 128
            && self
                .linux_user
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b"_-$".contains(&c))
            && linux_path(&self.service_data_dir)
            && linux_path(&self.default_data_dir)
            && self.default_data_dir.ends_with("/.solu")
            && self.original_path == format!("{}/claim.json", self.service_data_dir)
            && self.retired_path.starts_with(&retired_prefix)
            && self.retired_path[retired_prefix.len()..]
                .bytes()
                .all(|c| c.is_ascii_digit() || b"-TZ".contains(&c))
            && self.retired_path.len() > retired_prefix.len()
            && linux_path(&self.retired_path)
            && if self.shared() {
                self.claim_sha256
                    .as_deref()
                    .map(|v| lowercase_hex(v, 64))
                    .unwrap_or(false)
            } else {
                self.claim_sha256.is_none()
            };
        if valid {
            Ok(())
        } else {
            Err(invalid())
        }
    }

    pub(crate) fn phase_private(&self, phase: Phase) -> io::Result<Vec<u8>> {
        self.validate()?;
        if self.shared() {
            return Err(invalid());
        }
        serde_json::to_vec(&PhaseRecord {
            schema: 1,
            migration_id: self.migration_id.clone(),
            phase,
        })
        .map_err(|_| invalid())
    }

    pub(crate) fn shared(&self) -> bool {
        self.host_machine_id == self.wsl_machine_id
    }

    pub(crate) fn encode_private(&self) -> io::Result<Vec<u8>> {
        self.validate()?;
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        if bytes.len() > MAX_RECORD_BYTES {
            return Err(invalid());
        }
        Ok(bytes)
    }

    /// Phase evidence is accepted only for this exact migration. No phase can
    /// be inferred from a missing file or from a disconnect's 401/404 result.
    pub(crate) fn progress(
        &self,
        sending: Option<&[u8]>,
        confirmed: Option<&[u8]>,
    ) -> io::Result<crate::dual_migration_evidence::RevokeProgress> {
        use crate::dual_migration_evidence::RevokeProgress;
        if self.shared() {
            return if sending.is_none() && confirmed.is_none() {
                Ok(RevokeProgress::NotSending)
            } else {
                Err(invalid())
            };
        }
        match (sending, confirmed) {
            (None, None) => Ok(RevokeProgress::NotSending),
            (Some(sending), confirm) => {
                PhaseRecord::decode(sending, &self.migration_id, Phase::Sending)?;
                if let Some(confirm) = confirm {
                    PhaseRecord::decode(confirm, &self.migration_id, Phase::Confirmed204)?;
                    Ok(RevokeProgress::Confirmed204)
                } else {
                    Ok(RevokeProgress::Sending)
                }
            }
            (None, Some(_)) => Err(invalid()),
        }
    }

    /// The narrow 9e native owner-list continuation, deliberately restricted to
    /// Sending. This is not the general `distinct` evidence table, which also
    /// handles NotSending for the signed-in coordinator. Caller provenance and
    /// a successful complete owner response must be checked separately by Tauri.
    pub(crate) fn owner_list_continuation_reached(
        &self,
        progress: crate::dual_migration_evidence::RevokeProgress,
        status: u16,
        ids: &[String],
    ) -> io::Result<bool> {
        if ids.len() > 500 || ids.iter().any(|id| !identifier(id)) {
            return Err(invalid());
        }
        let unique: HashSet<_> = ids.iter().collect();
        if unique.len() != ids.len() {
            return Err(invalid());
        }
        Ok(!self.shared()
            && progress == crate::dual_migration_evidence::RevokeProgress::Sending
            && status == 200
            && unique.contains(&self.host_machine_id)
            && !unique.contains(&self.wsl_machine_id))
    }
}

#[derive(Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub(crate) enum Phase {
    Sending,
    Confirmed204,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct PhaseRecord {
    schema: u32,
    migration_id: String,
    phase: Phase,
}

impl PhaseRecord {
    fn decode(bytes: &[u8], migration_id: &str, phase: Phase) -> io::Result<Self> {
        if bytes.len() > 512 {
            return Err(invalid());
        }
        let record: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        if record.schema != 1 || record.migration_id != migration_id || record.phase != phase {
            return Err(invalid());
        }
        Ok(record)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dual_migration_evidence::RevokeProgress;
    fn bytes(shared: bool) -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "schema":1, "migrationId":"a".repeat(32), "hostMachineId":"host",
            "wslMachineId":if shared {"host"} else {"linux"}, "distro":"SoluTerminalRig",
            "linuxUser":"solurig", "serviceDataDir":"/home/solurig/.solu", "defaultDataDir":"/home/solurig/.solu",
            "originalPath":"/home/solurig/.solu/claim.json", "retiredPath":"/home/solurig/.solu/claim.json.retired-20261120",
            "claimSha256":if shared { Some("b".repeat(64)) } else {None}
        })).unwrap()
    }
    fn phase(id: &str, phase: Phase) -> Vec<u8> {
        serde_json::to_vec(&PhaseRecord {
            schema: 1,
            migration_id: id.into(),
            phase,
        })
        .unwrap()
    }
    #[test]
    fn private_intent_roundtrips_without_loosening_bindings() {
        for shared in [false, true] {
            let intent = Intent::decode(&bytes(shared)).unwrap();
            assert_eq!(intent.shared(), shared);
            let again = Intent::decode(&intent.encode_private().unwrap()).unwrap();
            assert_eq!(again.service_data_dir, "/home/solurig/.solu");
        }
    }
    #[test]
    fn malformed_or_unbound_intent_never_exposes_content() {
        let original: serde_json::Value = serde_json::from_slice(&bytes(true)).unwrap();
        for (key, value) in [
            ("schema", serde_json::json!(2)),
            ("migrationId", serde_json::json!("stale")),
            ("originalPath", serde_json::json!("/another/claim.json")),
            ("serviceDataDir", serde_json::json!("/home/../other")),
            (
                "retiredPath",
                serde_json::json!("/another/claim.json.retired-20261120"),
            ),
            ("claimSha256", serde_json::json!("private-fixture-value")),
            ("unexpected", serde_json::json!("private-fixture-value")),
        ] {
            let mut value_copy = original.clone();
            value_copy[key] = value;
            let error = Intent::decode(&serde_json::to_vec(&value_copy).unwrap())
                .err()
                .unwrap();
            assert!(!error.to_string().contains("private-fixture-value"));
        }
        assert!(Intent::decode(&vec![b'x'; MAX_RECORD_BYTES + 1]).is_err());
        let duplicate = String::from_utf8(bytes(true))
            .unwrap()
            .replacen("{", "{\"schema\":1,", 1);
        assert!(Intent::decode(duplicate.as_bytes()).is_err());
    }
    #[test]
    fn phase_evidence_is_monotonic_and_bound_to_one_distinct_intent() {
        let intent = Intent::decode(&bytes(false)).unwrap();
        let send = intent.phase_private(Phase::Sending).unwrap();
        let confirm = intent.phase_private(Phase::Confirmed204).unwrap();
        assert_eq!(
            intent.progress(None, None).unwrap(),
            RevokeProgress::NotSending
        );
        assert_eq!(
            intent.progress(Some(&send), None).unwrap(),
            RevokeProgress::Sending
        );
        assert_eq!(
            intent.progress(Some(&send), Some(&confirm)).unwrap(),
            RevokeProgress::Confirmed204
        );
        assert!(intent.progress(None, Some(&confirm)).is_err());
        assert!(intent
            .progress(Some(&phase(&"c".repeat(32), Phase::Sending)), None)
            .is_err());
        assert!(intent.progress(Some(&confirm), None).is_err());
        assert!(Intent::decode(&bytes(true))
            .unwrap()
            .progress(Some(&send), None)
            .is_err());
    }
    #[test]
    fn owner_list_continuation_only_retires_never_resumes() {
        let intent = Intent::decode(&bytes(false)).unwrap();
        for (progress, status, ids, expected) in [
            (RevokeProgress::Sending, 200, vec!["host".into()], true),
            (
                RevokeProgress::Sending,
                200,
                vec!["host".into(), "linux".into()],
                false,
            ),
            (RevokeProgress::Sending, 401, vec!["host".into()], false),
            (RevokeProgress::Sending, 200, vec!["other".into()], false),
            (RevokeProgress::NotSending, 200, vec!["host".into()], false),
            (
                RevokeProgress::Confirmed204,
                200,
                vec!["host".into()],
                false,
            ),
        ] {
            assert_eq!(
                intent
                    .owner_list_continuation_reached(progress, status, &ids)
                    .unwrap(),
                expected
            );
        }
        for ids in [
            vec!["host".into(), "host".into()],
            vec!["bad/id".into()],
            vec!["host".into(); 501],
        ] {
            assert!(intent
                .owner_list_continuation_reached(RevokeProgress::Sending, 200, &ids)
                .is_err());
        }
    }
}
