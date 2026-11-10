import { confirmDialog } from './dialog';

/** The one sign-out confirmation, shared by Settings › Account and the
 *  sidebar footer so both say the same thing about what stops. */
export function confirmSignOut(): Promise<boolean> {
  return confirmDialog({
    message: 'Sign out? Your panes keep running. Phone access, push alerts, voice and smart approvals stop until you sign in again.',
    confirmLabel: 'Sign out',
  });
}
