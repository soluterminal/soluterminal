import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { SystemStatus } from '../../api';
import { useStore } from '../../store';
import { useEntitlements } from '../../entitlements';
import { SystemPage } from './SystemPage';

const mocks=vi.hoisted(()=>({engineAvailable:true,activation:vi.fn().mockResolvedValue([]),snapshot:{status:null as SystemStatus|null,error:null as null|string,polling:false},refresh:vi.fn(),hostRefresh:vi.fn().mockResolvedValue({available:true}),usage:vi.fn().mockResolvedValue(null)}));

vi.mock('../../lib/useSystemStatus',()=>({SYSTEM_POLL_MS:10_000,useSystemStatus:()=>({...mocks.snapshot,lastOkMs:0}),refreshSystemStatus:mocks.refresh}));
vi.mock('../../lib/useDaemonHealth',async(importOriginal)=>{const actual=await importOriginal<typeof import('../../lib/useDaemonHealth')>();return {...actual,useDaemonHealth:()=>({health:{version:'1'}})};});
vi.mock('../../lib/useNow',()=>({useNow:()=>Date.parse('2026-01-01T00:00:10Z')}));
vi.mock('../../api',async(importOriginal)=>{const actual=await importOriginal<typeof import('../../api')>();return {...actual,api:{...actual.api,ccUsageSummaryCached:mocks.usage,hostStorageRefresh:mocks.hostRefresh,workflowActivation:mocks.activation}};});

function fixture():SystemStatus{return {sampledAt:'2026-01-01T00:00:00Z',treeSampledAt:'2026-01-01T00:00:00Z',generatedAt:'2026-01-01T00:00:00Z',sampleIntervalSec:15,platform:{os:'linux',procfs:true},level:{overall:'warn',reason:'disk: 4.0 GB free',reasons:[],areas:{daemonMemory:'ok',hostMemory:'ok',disk:'warn',engine:'ok'}},daemon:{version:'1',webVersion:'1',backendID:'wsl',shellVersion:'1',pid:1,uptimeSec:10,startedAt:'',rssMB:512,goHeapMB:100,goroutines:10,cpuPct:2,memGuardMB:6144},host:{cores:8,cpuPct:4,loadAvg1:1,memTotalMB:8192,memAvailableMB:4096,memFloorMB:2048},footprint:{totalRssMB:1024,daemonRssMB:512,panesRssMB:512,workersRssMB:0,paneCount:1,workerCount:0,procs:3,cpuPct:2,complete:true},panes:[],workers:[],disk:{layer:'local',blockingLayer:'none',anyLow:false,host:null,path:'~/.solu',freeMB:4096,totalMB:10000,floorMB:2048,low:false,measuredAt:null,breakdown:null,breakdownAt:null,measuring:true,fresh:false,nextMeasureAfter:null,goCacheBytes:null,worktreesBytes:null},engine:{available:true,queue:{cap:4,running:1,runnable:0,capacityQueued:0,dependencyBlocked:0,overlapSerialized:0,awaitingApproval:0,quotaPaused:0,landingWait:0,resourceWait:0,heavyCap:2,heavyRunning:0,resourceDeferrals:0,memFloorMB:2048,diskFloorMB:2048,diskFreeMB:4096,diskLow:false,utilization:.25},runningInvocations:0,decide:2,gcLastSweepAt:null},runtimes:null,automation:{agents:{total:1,paused:0,running:0,blocked:0,errors24h:0,nextRunAt:null},schedules:{enabled:1,disabled:0,running:0,errors24h:0,nextFireAt:null},builtins:[],autonomy:{globalPaused:false,pausedCount:0},warden:{enabled:false,lastRunAt:null,lastRunStatus:''}},surface:{addr:'127.0.0.1:8787',ports:{total:1,paneOwned:1},previews:{active:0},cloud:{configured:false,hasCreds:false,credsInvalid:false,relayState:'unconfigured'}},release:{running:'1',latest:'1',available:false,applying:false,lastSuccessMs:0,lastError:'',shellCurrent:'1',shellLatest:'1',shellAvailable:false,readiness:null}};}

beforeEach(()=>{mocks.engineAvailable=true;useEntitlements.setState({loaded:true,tier:'team',team: true, operator: true,account:true});});
afterEach(()=>{mocks.snapshot={status:null,error:null,polling:false};useStore.setState({});vi.clearAllMocks();});
describe('SystemPage',()=>{
  it('keeps System services visible without polling workflow activation when shelved',()=>{
    mocks.engineAvailable=false;
    const status=fixture();status.engine.available=false;
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('Crew & runtimes')).toBeInTheDocument();
    expect(screen.getByText('Release & update')).toBeInTheDocument();
    expect(screen.getByRole('button',{name:'Agents status'})).toBeInTheDocument();
    expect(screen.queryByRole('button',{name:'Managed agents'})).not.toBeInTheDocument();
    expect(screen.getByRole('button',{name:'Schedules'})).toBeInTheDocument();
    expect(screen.queryByRole('button',{name:'Open Tasks › Engine'})).not.toBeInTheDocument();
    expect(mocks.activation).not.toHaveBeenCalled();
  });

  it('renders KPI hierarchy and verdict',()=>{mocks.snapshot={status:fixture(),error:null,polling:false};render(<SystemPage onClose={()=>{}}/>);expect(screen.getByText('Footprint')).toBeInTheDocument();expect(screen.getByText('Host memory')).toBeInTheDocument();expect(screen.getAllByText('Disk').length).toBeGreaterThan(0);expect(screen.getByText('Engine')).toBeInTheDocument();expect(screen.getByText('disk: 4.0 GB free')).toBeInTheDocument();});
  it('explains when a requested disk measurement is served from the fresh cache',()=>{
    const status=fixture();
    status.disk.measuring=false;
    status.disk.fresh=true;
    status.disk.breakdown=[];
    status.disk.breakdownAt='2025-12-31T23:58:10Z';
    status.disk.nextMeasureAfter='2026-01-01T00:08:10Z';
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('measured 2m ago · fresh cache')).toBeInTheDocument();
  });
  it('renders WSL and Windows-host provenance',()=>{
    const status=fixture();
    status.host.provenance='wsl';
    status.host.windows={status:'stale',freeMB:3072,totalMB:65536,ageSec:120,provenance:'windows'};
    status.host.pressure={at:'2026-01-01T00:00:00Z',memPSISome10:null,memPSIFull10:0,cpuPSISome60:2,ioPSIFull60:1,swapOutPerSec:0,loadAvg1:3,nproc:24,provenance:'wsl'};
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('WSL VM memory')).toBeInTheDocument();
    expect(screen.getByText(/Windows host: 3.00 GB free \(stale · 120s old\)/)).toBeInTheDocument();
  });
  it('separates RSS, allocated heap and guard accounting',()=>{
    const status=fixture();
    status.daemon.rssMB=2048;status.daemon.goHeapMB=1536;status.daemon.goRuntimeMB=4608;status.daemon.guardUsedMB=4608;
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText(/Daemon RSS 2.00 GB · allocated Go heap 1.50 GB · Go committed 4.50 GB · guard accounting 4.50 GB \/ 6.00 GB/)).toBeInTheDocument();
  });
  it('renders old daemon state',()=>{mocks.snapshot={status:null,error:'old-daemon',polling:false};render(<SystemPage onClose={()=>{}}/>);expect(screen.getByText(/predates the System page/)).toBeInTheDocument();});
  it('closes on escape and direct backdrop click',()=>{mocks.snapshot={status:fixture(),error:null,polling:false};const close=vi.fn();const {container}=render(<SystemPage onClose={close}/>);fireEvent.keyDown(window,{key:'Escape'});fireEvent.click(document.querySelector('.system-page-backdrop')!);expect(close).toHaveBeenCalledTimes(2);expect(container).toBeTruthy();});
  it('renders host-vs-guest storage and refreshes the probe',async()=>{const status=fixture();status.disk.layer='guest';status.disk.blockingLayer='host';status.disk.anyLow=true;status.disk.host={available:true,state:'ok',detail:'',volume:'C:',distro:'Ubuntu',freeMB:41000,totalMB:2050000,freeBytes:41000*1024*1024,totalBytes:2050000*1024*1024,vhdxAllocatedBytes:566000*1024*1024,vhdxApparentBytes:1048576*1024*1024,floorMB:61500,floorPct:3,hysteresisMB:4096,low:true,vhdxPath:'~\\ext4.vhdx',vhdxPhysicalMB:566000,vhdxLogicalMB:1048576,sparse:false,sampledAt:'2026-01-01T00:00:00Z',runbook:'docs/RUNBOOK-wsl-host-disk.md'};mocks.snapshot={status,error:null,polling:false};render(<SystemPage onClose={()=>{}}/>);expect(screen.getByText(/blocked by the Windows host disk/)).toBeInTheDocument();expect(screen.getByText('Host storage')).toBeInTheDocument();expect(screen.getByText(/ext4.vhdx .* allocated .* apparent/)).toBeInTheDocument();fireEvent.click(screen.getByRole('button',{name:'Refresh probe'}));expect(mocks.hostRefresh).toHaveBeenCalled();});
  it('renders storage producers, read-only policy and guest-only cleanup',()=>{
    const status=fixture();
    status.disk.breakdown=[
      {name:'workflow_worktrees',category:'workflow_worktrees',label:'Workflow worktrees',bytes:100,apparentBytes:100,allocatedBytes:80,complete:true,measured:true,protection:'managed',cleanup:'workflow_gc',scannedAt:'2026-01-01T00:00:00Z',targetCount:2},
      {name:'external_user_cache',category:'external_user_cache',label:'External user cache',bytes:900,apparentBytes:900,complete:false,measured:true,protection:'read-only',cleanup:'none',reasons:['entry_limit']},
      {name:'protected_recovery_data',category:'protected_recovery_data',label:'Protected recovery data',bytes:500,apparentBytes:500,complete:true,measured:true,protection:'protected',cleanup:'none'},
    ];
    status.disk.growth={day:{window:'24h',available:false,continuity:'reboot'},week:{window:'168h',available:true,continuity:'ok',deltas:[{category:'workflow_worktrees',label:'Workflow worktrees',baselineBytes:50,latestBytes:100,deltaBytes:50,noise:false}]}};
    status.disk.cleanup={complete:true,previewOnly:false,scope:'0 eligible under current workflow GC policy',operations:[{at:'2026-01-01T00:00:00Z',kind:'worktree_gc',layer:'guest',executed:true,dryRun:false,complete:true,verified:false,guestFreeDeltaBytes:123}],skips:[{reason:'within retention window',count:3}],state:'guest_only_pending_host_reclaim',message:'Linux space freed; Windows image still allocated; maintenance needed'};
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('Storage producers')).toBeInTheDocument();
    expect(screen.getByText('External user cache')).toBeInTheDocument();
    expect(screen.getByText('read-only')).toBeInTheDocument();
    expect(screen.getByText('protected')).toBeInTheDocument();
    expect(screen.getByText(/Linux space freed; Windows image still allocated; maintenance needed/)).toBeInTheDocument();
    expect(screen.getByText('Cleanup & reclamation')).toBeInTheDocument();
    expect(screen.getByText(/within retention window/)).toBeInTheDocument();
  });
  it('shows strangers resources and updates, not the team engine, release train or runbooks',()=>{
    useEntitlements.setState({loaded:true,tier:'none',team: false, operator: false,account:false});
    const status=fixture();
    status.disk.host={available:true,state:'ok',detail:'',volume:'C:',distro:'Ubuntu',freeMB:41000,totalMB:2050000,freeBytes:0,totalBytes:0,floorMB:61500,floorPct:3,hysteresisMB:4096,low:false,vhdxPath:'',vhdxPhysicalMB:1,vhdxLogicalMB:1,sparse:false,sampledAt:'2026-01-01T00:00:00Z',runbook:'docs/RUNBOOK-wsl-host-disk.md'};
    status.level={overall:'warn',reason:'runtimes: 10 unhealthy runtimes',reasons:[{key:'runtimes',level:'warn',text:'runtimes: 10 unhealthy runtimes'}],areas:{daemonMemory:'ok',hostMemory:'ok',disk:'ok',engine:'unknown',runtimes:'warn'}};
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('Processes')).toBeInTheDocument();
    expect(screen.getByText('Updates')).toBeInTheDocument();
    expect(screen.getByRole('button',{name:'Open update status'})).toBeInTheDocument();
    for (const hidden of ['Workflow engine','Crew & runtimes','Automation','Release & update','Cleanup & reclamation']) expect(screen.queryByText(hidden)).toBeNull();
    expect(screen.queryByRole('button',{name:'Manage worktrees'})).toBeNull();
    expect(screen.queryByText(/RUNBOOK/)).toBeNull();
    expect(screen.getByText('All monitored systems healthy')).toBeInTheDocument();
    expect(screen.queryByText(/unhealthy runtimes/)).toBeNull();
    expect(screen.queryByText('Engine')).toBeNull();
  });
  it('speaks plainly to strangers: no daemon names, Go memory, VHDX, runbooks or listener',async()=>{
    useEntitlements.setState({loaded:true,tier:'none',team: false, operator: false,account:false});
    const status=fixture();
    status.disk.layer='guest';status.disk.blockingLayer='host';status.disk.anyLow=true;
    status.disk.goCacheBytes=1024;
    status.disk.breakdown=[{name:'external_user_cache',category:'external_user_cache',label:'External user cache',bytes:900,apparentBytes:900,complete:true,measured:true,protection:'read-only',cleanup:'none'}];
    status.disk.host={available:true,state:'ok',detail:'',volume:'C:',distro:'Ubuntu',freeMB:41000,totalMB:2050000,freeBytes:41000*1024*1024,totalBytes:2050000*1024*1024,vhdxAllocatedBytes:1,vhdxApparentBytes:1,floorMB:61500,floorPct:3,hysteresisMB:4096,low:true,vhdxPath:'~\\ext4.vhdx',vhdxPhysicalMB:1,vhdxLogicalMB:1,sparse:false,sampledAt:'2026-01-01T00:00:00Z',runbook:'docs/RUNBOOK-wsl-host-disk.md',offender:{label:'WSL dumps',path:'',mb:1,files:1,dumps:1,complete:true,text:'51.0 GB of WSL crash dumps'}};
    status.level={overall:'warn',reason:'disk: 4.0 GB free on ~/.solu volume',reasons:[{key:'disk',level:'warn',text:'disk: 4.0 GB free on ~/.solu volume'}],areas:{disk:'warn'}};
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    await Promise.resolve();
    // 41 GB free on a 2 TB drive blocks heavy work but is not "almost full".
    expect(screen.getByText('Disk almost full')).toBeInTheDocument();
    expect(screen.queryByText(/Your Windows drive C: is almost full/)).not.toBeInTheDocument();
    expect(screen.getByText(/Some background work waits until your Windows drive C: has more space/)).toBeInTheDocument();
    expect(screen.getByText('SoluTerminal v1')).toBeInTheDocument();
    expect(screen.getByText('Windows disk')).toBeInTheDocument();
    expect(screen.getByText('Sessions')).toBeInTheDocument();
    expect(screen.getByText('Connections')).toBeInTheDocument();
    const text=document.body.textContent ?? '';
    for (const jargon of [/solud/,/Go heap/,/guard accounting/,/ext4\.vhdx/,/VHDX/,/RUNBOOK/,/host-storage/,/~\/\.solu/,/engine floor/,/Listener/,/Previews/,/Storage producers/,/Go build cache/,/Usage capture needs cloud pairing/,/Usage · 7 days/,/workers/]) {
      expect({jargon:String(jargon),found:jargon.test(text)}).toEqual({jargon:String(jargon),found:false});
    }
  });
  it('explains a missing sampler to strangers without kill-switch names',()=>{
    useEntitlements.setState({loaded:true,tier:'none',team: false, operator: false,account:false});
    mocks.snapshot={status:null,error:'unavailable',polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText('Resource monitoring is turned off on this computer.')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/SOLU_SYSTEM_SAMPLER|solud/);
  });
  it('keeps engineering detail for a team plan without operator tooling',()=>{
    useEntitlements.setState({loaded:true,tier:'team',team: true, operator: false,account:true});
    const status=fixture();
    mocks.snapshot={status,error:null,polling:false};
    render(<SystemPage onClose={()=>{}}/>);
    expect(screen.getByText(/solud v1/)).toBeInTheDocument();
    expect(screen.getByText(/allocated Go heap/)).toBeInTheDocument();
    expect(screen.getByText('Listener')).toBeInTheDocument();
    expect(screen.getByText('Usage capture needs cloud pairing.')).toBeInTheDocument();
    expect(screen.queryByText('Workflow engine')).toBeNull();
  });
});
