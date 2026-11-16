import type { StandalonePage } from '../../standalonePages';

// Self-serve account deletion (ST-174). soluterminal.com/account/delete links
// here; it signs in with the hosted app's own sign-in.
const page: StandalonePage = {
  path: '/account/delete',
  load: () => import('./AccountDeletePage'),
};

export default page;
