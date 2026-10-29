import {buildAmbientAppObjectResponses,buildServiceObjectResponses} from './screens-lib.mjs';
// Reads the app shell makes at boot or when a sidebar section opens; an
// isolated preview answers them as an empty install rather than a 404.
const emptyInstall={'/api/agents':[],'/api/agents/reports':[],'/api/schedules':[],'/api/orchestrator-hosts':{policyVersion:'',hosts:[]}};
export function buildPreviewFixtures(version){return {...emptyInstall,...buildAmbientAppObjectResponses(version),...buildServiceObjectResponses()};}
