import { titleShowsHandle } from '../lib/paneHeaderDetails';
import { useState } from 'react';
import type { PaneInfo } from '../types';
import { PaneIdentityDialog, workingOnProvenance } from './PaneIdentityDialog';

// The pane header's "@api-fix · Fixing the login redirect": the handle you
// and the agents address it by, and what it is doing now. Nothing when
// neither is set. Clicking opens the editor.
export function PaneIdentityChip({ pane, deduplicateTitle = false }: { pane: PaneInfo; deduplicateTitle?: boolean }) {
  const [editing, setEditing] = useState(false);
  const handle = pane.identity?.handle;
  const workingOn = pane.identity?.workingOn;
  if (!editing && ((!handle && !workingOn) || (deduplicateTitle && titleShowsHandle(pane)))) return null;
  const provenance = workingOnProvenance(pane);
  const tip = [
    handle && `@${handle} — use it in place of the pane number`,
    workingOn && `Working on: ${workingOn}${provenance ? ` (${provenance})` : ''}`,
  ].filter(Boolean).join('\n');
  return (
    <>
      {(handle || workingOn) && (
        <button
          type="button"
          className="pane-identity-chip"
          title={tip}
          aria-label={`${tip}. Edit`}
          onClick={(e) => { e.stopPropagation(); setEditing(true); }}
          onMouseDown={(e) => e.stopPropagation()}
          onDoubleClick={(e) => e.stopPropagation()}
        >
          {handle && <span className="pane-identity-handle">@{handle}</span>}
          {workingOn && <span className="pane-identity-working-on">{workingOn}</span>}
        </button>
      )}
      {editing && <PaneIdentityDialog pane={pane} onClose={() => setEditing(false)} />}
    </>
  );
}
