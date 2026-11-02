import type { Dispatch, SetStateAction } from 'react';
import { useStore } from '../store';
import { IconFlag } from './ui/icons';
import type { PaneInfo } from '../types';

interface PaneFlagMemoProps {
  pane: PaneInfo;
  actionGuard?:()=>boolean;
  onSave?:(flagged:boolean,memo:string)=>Promise<unknown>;
  editing: boolean;
  draft: string;
  setEditing: Dispatch<SetStateAction<boolean>>;
  setDraft: Dispatch<SetStateAction<string>>;
}

export function PaneFlagMemo({
  pane, actionGuard=()=>true, onSave,
  editing,
  draft,
  setEditing,
  setDraft,
}: PaneFlagMemoProps) {
  const save=(flagged:boolean,memo:string)=>onSave ? onSave(flagged,memo) : useStore.getState().setPaneFlag(pane.id,flagged,memo);
  if (!pane.flagged) return null;

  return (
    <div className="pane-flag-memo" role="note">
      <span className="pane-flag-memo-icon" aria-hidden><IconFlag size={13} style={{ verticalAlign: '-2px' }} /></span>
      {editing ? (
        <input
          className="pane-flag-memo-input"
          autoFocus
          placeholder="What needs your attention here?"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={async () => {
            if(!actionGuard()) {setEditing(false);return;}
            setEditing(false);
            const next = draft.trim();
            if (next !== (pane.flagMemo ?? '')) {
              try { await save(true,next); } catch {}
            }
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
            if (e.key === 'Escape') {
              setDraft(pane.flagMemo ?? '');
              setEditing(false);
            }
            e.stopPropagation();
          }}
          onMouseDownCapture={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        />
      ) : (
        <span
          className="pane-flag-memo-text"
          title="Click to edit memo · right-click pane → Unflag to clear"
          onClick={(e) => {
            e.stopPropagation();
            setDraft(pane.flagMemo ?? '');
            setEditing(true);
          }}
        >
          {pane.flagMemo || 'Flagged for attention — click to add a memo'}
        </span>
      )}
      <button
        className="pane-flag-memo-clear"
        title="Unflag"
        onClick={(e) => {
          e.stopPropagation();
          if(actionGuard())void save(false,'').catch(() => {});
        }}
      >×</button>
    </div>
  );
}
