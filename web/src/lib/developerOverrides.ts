import { create } from 'zustand';
import { api } from '../api';

// The person's Developer overrides switch (daemon/developer_overrides.go),
// shared by its Settings row and the status-bar indicator. refresh() runs on
// mount and on the daemon's "developer-overrides" event, so a change from
// Settings or an update-channel switch shows at once.
interface DeveloperOverridesState {
  loaded: boolean;
  on: boolean;
  required: boolean;
  refresh: () => Promise<void>;
  set: (on: boolean) => Promise<void>;
}

export const useDeveloperOverrides = create<DeveloperOverridesState>((set) => ({
  loaded: false,
  on: false,
  required: false,
  refresh: async () => {
    try {
      const s = await api.getDeveloperOverrides();
      if (s) set({ loaded: true, on: s.on, required: s.required });
    } catch {
      /* older or unreachable daemon: nothing to show */
    }
  },
  set: async (on: boolean) => {
    const s = await api.setDeveloperOverrides(on);
    set({ loaded: true, on: s.on, required: s.required });
  },
}));
