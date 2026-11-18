import { useCloud } from '../cloud';
import { downloadCountTag, trackingRefused } from './arrival';

// PostgREST's answer when no function matches the arguments: the counter
// doesn't take p_path yet (the column is added by a separate migration).
const NO_MATCHING_FUNCTION = 'PGRST202';

// A download started from the hosted app skips the website's counter, so
// record it with the same insert-only RPC and the same first-touch tag, marked
// as the account path (the website's direct downloads are "direct"). Fire and
// forget: it never delays or blocks the download, and errors are ignored.
export function countSiteDownload(): void {
  const { supabase, isLocal } = useCloud.getState();
  if (!supabase || isLocal) return;
  // Do Not Track, Global Privacy Control and automated browsers: not counted.
  if (trackingRefused()) return;
  const source = downloadCountTag();
  try {
    void Promise.resolve(
      supabase.rpc('record_soluterminal_download', { p_source: source, p_path: 'account' }),
    )
      .then((res) => {
        if (res?.error?.code === NO_MATCHING_FUNCTION) {
          return supabase.rpc('record_soluterminal_download', { p_source: source });
        }
        return undefined;
      })
      .catch(() => {});
  } catch {
    // Never let counting affect the download.
  }
}
