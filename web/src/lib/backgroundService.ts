import { create } from 'zustand';
import { backgroundServiceClient } from '../api';
import { confirmDialog } from './dialog';

export interface ServiceCapability { supported: boolean; pid: number; method?: string; backendID?: string; daemonIdentity?: string; heldDeliveries?: number; putBackReceipts?: number }
export const useBackgroundService = create<{ pending: boolean; message: string; error: boolean }>(() => ({ pending: false, message: '', error: false }));

export async function backgroundServiceCapability(): Promise<ServiceCapability> {
  const res = await backgroundServiceClient()('/api/service/restart');
  if (!res.ok) return { supported: false, pid: 0 };
  return res.json();
}

// Polling only observes recovery. The mutation is never retried, including
// uncertain delivery; an explicit subsequent click requires fresh consent.
export async function restartBackgroundService(): Promise<void> {
  if (useBackgroundService.getState().pending) return;
  useBackgroundService.setState({ pending: true, error: false, message: 'Restarting background service… Panes will reconnect.' });
  const request = backgroundServiceClient();
  try {
    const capabilityResponse = await request('/api/service/restart');
    if (!capabilityResponse.ok) throw new Error('Background service restart is unavailable.');
    const capability: ServiceCapability = await capabilityResponse.json();
    if (!capability.supported || !capability.daemonIdentity) throw new Error('No safe service restart path is available on this computer.');
    if (!await confirmDialog({ message: `Restart the background service? Panes keep running and reconnect. Changes to the service env file take effect. ${capability.heldDeliveries ?? 0} held deliveries and ${capability.putBackReceipts ?? 0} put-back receipts may be lost (counts at confirmation).` })) return;
    const res = await request('/api/service/restart', { method: 'POST', body: JSON.stringify({ pid: capability.pid }), headers: { 'Content-Type': 'application/json' } });
    if (!res.ok) throw new Error(await res.text());
    const deadline = Date.now() + 90_000;
    while (Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 1500));
      try {
        const health = await request('/api/health?identity=1', { cache: 'no-store', signal: AbortSignal.timeout(Math.min(20_000, Math.max(1, deadline - Date.now()))) });
        if (!health.ok) continue;
        const identity = await health.json();
        if (Number.isInteger(identity.pid) && identity.pid > 0 && identity.pid !== capability.pid && identity.backendID === capability.backendID && identity.daemonIdentity === capability.daemonIdentity) {
          useBackgroundService.setState({ message: `Background service restarted. PID ${capability.pid} → ${identity.pid}.`, error: false });
          return;
        }
      } catch { /* A disconnected daemon is expected during this bounded wait. */ }
    }
    throw new Error('The background service has not returned with a new PID. No further restart was sent.');
  } catch (error) {
    useBackgroundService.setState({ error: true, message: `Could not confirm restart: ${error instanceof Error ? error.message : String(error)} No automatic retry.` });
  } finally {
    useBackgroundService.setState({ pending: false });
    if (useBackgroundService.getState().message.startsWith('Restarting')) useBackgroundService.setState({ message: '' });
  }
}
