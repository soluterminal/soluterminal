import { useState } from 'react';
import type { WorkflowSettings } from '../../api';
import type { WorkflowSettingsUpdate } from './ServicesTab';
import { SettingsGroup, SettingsRow } from './SettingsRow';
type Resources=NonNullable<WorkflowSettings['resources']>;
const knobs=[
 {key:'maxAgentRuns',label:'Max agent runs',min:1,max:8,hint:'Automation runs share this pool.'},
 {key:'memFloorMB',label:'Memory floor',min:0,hint:'New release operations wait below this many available MB. 0 disables the gate.'},
 {key:'diskFloorMB',label:'Disk floor',min:0,hint:'New release operations wait below this many free MB. 0 disables the gate.'},
 {key:'hostDiskFloorMB',label:'Host disk floor (WSL)',min:0,hint:'Minimum free MB on the Windows volume holding the WSL disk.'},
 {key:'hostDiskFloorPct',label:'Host disk floor %',min:0,max:50,hint:'The larger of the absolute and percentage host floors applies.'},
] as const;
// Opt-ins the daemon also reads from the environment: "Default" leaves the
// environment (or built-in default) in charge, On/Off override it here.
const optIns=[
 {key:'adaptiveHostDiskFloor',label:'Adaptive host disk floor',env:'SOLU_WORKFLOW_HOST_DISK_ADAPTIVE_FLOOR',hint:'Opt in to a floor based on measured disk growth.'},
 {key:'rotateWSLCrashDumps',label:'Rotate WSL crash dumps',env:'SOLU_WORKFLOW_ROTATE_WSL_CRASH_DUMPS',hint:'Keep the two newest dumps; older dumps can be deleted when host pressure requires it.'},
] as const;
export function ResourceLimitsSection({sharedSettings,updateSharedSettings}:{sharedSettings:WorkflowSettings;updateSharedSettings:WorkflowSettingsUpdate}) {
 const [error,setError]=useState('');
 const saved=sharedSettings.resources;
 const update=async(key:keyof Resources,value:number|boolean|undefined)=>{try{await updateSharedSettings(s=>({...s,resources:{...s.resources,[key]:value}}));setError('');return true}catch(e){setError(String(e));return false}};
 return <SettingsGroup title="Resource limits">
 {knobs.map(k=>{
  const current=saved?.[k.key];
  // Keyed by the saved value: the box shows what is saved, also after a
  // change made elsewhere, and a rejected edit goes back to it.
  return <SettingsRow key={k.key} label={k.label} hint={k.hint}><input key={`${k.key}:${current??''}`} aria-label={k.label} type="number" min={k.min} max={'max' in k?k.max:undefined} placeholder="Inherit environment / default" defaultValue={current??''} onBlur={async e=>{
   const input=e.currentTarget;const raw=input.value.trim();
   if(raw===String(current??''))return;
   const n=raw===''?undefined:Number(raw);
   if(n!==undefined&&!(Number.isInteger(n)&&n>=k.min&&(!('max' in k)||n<=k.max))){input.value=String(current??'');setError(`${k.label} must be a whole number${'max' in k?` from ${k.min} to ${k.max}`:` of at least ${k.min}`}; kept ${current??'the default'}.`);return}
   // A failed save leaves the saved value as it was: show it again.
   if(!await update(k.key,n)&&input.isConnected)input.value=String(current??'');
  }}/></SettingsRow>})}
 {optIns.map(o=>{
  const v=saved?.[o.key];
  return <SettingsRow key={o.key} label={o.label} hint={`${o.hint} Default follows ${o.env} (off when unset).`}><select aria-label={o.label} value={v===undefined?'default':v?'on':'off'} onChange={e=>{const next=e.currentTarget.value;void update(o.key,next==='default'?undefined:next==='on')}}><option value="default">Default</option><option value="on">On</option><option value="off">Off</option></select></SettingsRow>})}
 {error&&<p role="alert">{error}</p>}
 </SettingsGroup>;
}
