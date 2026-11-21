//! Pure recovery table for ST-253 ruling 9e, including the Windows durability
//! amendment. No filesystem, claims, credentials, network or mutation here.

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Decision {
    Before,
    Reached { retry_revoke: bool },
    Blocked,
}

/// The future probe compares against the hash bound into the private intent.
/// A missing/unreadable/mismatched claim is not positive retirement evidence.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ClaimFile {
    Absent,
    Matching,
    Other,
    Unreadable,
}

pub(crate) fn shared(original: ClaimFile, retired: ClaimFile) -> Decision {
    match (original, retired) {
        (ClaimFile::Matching, ClaimFile::Absent) => Decision::Before,
        (ClaimFile::Absent, ClaimFile::Matching) => Decision::Reached {
            retry_revoke: false,
        },
        _ => Decision::Blocked,
    }
}

/// These phases must come from a validated, bound and durably published intent.
/// Confirmed204 cannot be constructed from a disconnect's "unlisted" outcome.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RevokeProgress {
    NotSending,
    Sending,
    Confirmed204,
}

/// When signed in the coordinator MUST attempt the person's owner-list GET,
/// even at NotSending. Unavailable includes non-200, ambiguous or failed reads;
/// neither a machine-token response nor a revoke 401/404 can construct Owner200.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum OwnerList {
    SignedOut,
    Unavailable,
    Owner200 {
        host_present: bool,
        wsl_present: bool,
    },
}

pub(crate) fn distinct(progress: RevokeProgress, owner: OwnerList) -> Decision {
    // A confirmed point of no return is monotonic, including after re-pair.
    if progress == RevokeProgress::Confirmed204 {
        return Decision::Reached {
            retry_revoke: false,
        };
    }
    match owner {
        OwnerList::Owner200 {
            host_present: true,
            wsl_present: true,
        } if progress == RevokeProgress::NotSending => Decision::Before,
        OwnerList::Owner200 {
            host_present: true,
            wsl_present: false,
        } => Decision::Reached { retry_revoke: true },
        OwnerList::SignedOut if progress == RevokeProgress::NotSending => Decision::Before,
        _ => Decision::Blocked,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn every_shared_row_requires_exact_matching_and_absence() {
        use ClaimFile::*;
        for (original, expected) in [
            (
                Absent,
                [
                    Decision::Blocked,
                    Decision::Reached {
                        retry_revoke: false,
                    },
                    Decision::Blocked,
                    Decision::Blocked,
                ],
            ),
            (
                Matching,
                [
                    Decision::Before,
                    Decision::Blocked,
                    Decision::Blocked,
                    Decision::Blocked,
                ],
            ),
            (Other, [Decision::Blocked; 4]),
            (Unreadable, [Decision::Blocked; 4]),
        ] {
            for (retired, result) in [Absent, Matching, Other, Unreadable]
                .into_iter()
                .zip(expected)
            {
                assert_eq!(
                    shared(original, retired),
                    result,
                    "{original:?}/{retired:?}"
                );
            }
        }
    }
    #[test]
    fn every_distinct_row_uses_owner_evidence_even_before_sending() {
        use Decision::*;
        use RevokeProgress::*;
        let reached = Reached { retry_revoke: true };
        for (owner, expected) in [
            (OwnerList::SignedOut, [Before, Blocked]),
            (OwnerList::Unavailable, [Blocked, Blocked]),
            (
                OwnerList::Owner200 {
                    host_present: true,
                    wsl_present: false,
                },
                [reached, reached],
            ),
            (
                OwnerList::Owner200 {
                    host_present: true,
                    wsl_present: true,
                },
                [Before, Blocked],
            ),
            (
                OwnerList::Owner200 {
                    host_present: false,
                    wsl_present: true,
                },
                [Blocked, Blocked],
            ),
            (
                OwnerList::Owner200 {
                    host_present: false,
                    wsl_present: false,
                },
                [Blocked, Blocked],
            ),
        ] {
            for (progress, result) in [NotSending, Sending].into_iter().zip(expected) {
                assert_eq!(distinct(progress, owner), result, "{progress:?}/{owner:?}");
            }
            assert_eq!(
                distinct(Confirmed204, owner),
                Reached {
                    retry_revoke: false
                }
            );
        }
    }
    #[test]
    fn confirmed_revoke_is_monotonic_across_owner_observations() {
        assert_eq!(
            distinct(RevokeProgress::Sending, OwnerList::Unavailable),
            Decision::Blocked
        );
        for owner in [
            OwnerList::SignedOut,
            OwnerList::Unavailable,
            OwnerList::Owner200 {
                host_present: true,
                wsl_present: true,
            },
        ] {
            assert_eq!(
                distinct(RevokeProgress::Confirmed204, owner),
                Decision::Reached {
                    retry_revoke: false
                }
            );
        }
    }
}
