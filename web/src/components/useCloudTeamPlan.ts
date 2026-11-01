import { useEffect, useState } from 'react';

// Whether the signed-in hosted-app user has the team plan (cloud
// /api/entitlements). The machine picker shows the headless install commands
// only to the team: public visitors get the desktop app from the website, and
// the install script fetches from the release host, which stays private.
// False until the cloud answers, and on any error.
export function useCloudTeamPlan(bearer: string | null): boolean {
  const [team, setTeam] = useState(false);
  useEffect(() => {
    if (!bearer) {
      setTeam(false);
      return;
    }
    let live = true;
    void fetch('/api/entitlements', { headers: { Authorization: `Bearer ${bearer}` } })
      .then((res) => (res.ok ? res.json() : null))
      .then((body: { team?: unknown } | null) => {
        if (live) setTeam(body?.team === true);
      })
      .catch(() => {
        if (live) setTeam(false);
      });
    return () => {
      live = false;
    };
  }, [bearer]);
  return team;
}
