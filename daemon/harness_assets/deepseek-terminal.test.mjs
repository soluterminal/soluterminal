import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

// These tests are frequently run from inside a managed SoluTerminal pane, whose
// environment carries the daemon's resume/pane hints. The spawned client reads
// SOLU_HARNESS_RESUME_SESSION and would try to resume that session against the
// test's temp DSH_HOME, where it does not exist; the fake runtime never answers
// session/resume, so every interactive test hangs until its kill timer. Strip
// the inherited hints once here — tests that exercise resume set their own.
delete process.env.SOLU_HARNESS_RESUME_SESSION;
delete process.env.SOLU_PANE_ID;

test('terminal approval menu rejects on explicit no and closes the child after /exit', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-permission-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-test',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;send({id:99,method:'session/request_permission',params:{sessionId:'session-test',toolCall:{title:'Write a file',rawInput:{path:'test.txt'}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});}
else if(m.id===99){send({method:'session/update',params:{sessionId:'session-test',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:m.result.outcome.optionId==='deny'?'DENIED_CORRECTLY':'UNEXPECTED_ALLOW'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, answered = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('write a file\n'); }
      if (!answered && output.includes('Select [1/2]: ')) { answered = true; child.stdin.write('n\n'); }
      if (!exited && output.includes('DENIED_CORRECTLY') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('❯ 1. Yes, allow once'));
    assert.ok(output.includes('2. No, reject'));
    assert.ok(output.includes('DENIED_CORRECTLY'));
    assert.ok(!output.includes('UNEXPECTED_ALLOW'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('focused approval menu allows on bare Enter (the auto-approval keystroke)', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-auto-allow-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-auto',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;send({id:99,method:'session/request_permission',params:{sessionId:'session-auto',toolCall:{title:'Write a file',rawInput:{path:'test.txt'}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});}
else if(m.id===99){send({method:'session/update',params:{sessionId:'session-auto',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:m.result.outcome.optionId==='allow'?'ALLOWED_CORRECTLY':'UNEXPECTED_DENY'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, answered = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('write a file\n'); }
      // Bare Enter is exactly what the daemon's AutoMode direct-fire writes.
      if (!answered && output.includes('Select [1/2]: ')) { answered = true; child.stdin.write('\n'); }
      if (!exited && output.includes('ALLOWED_CORRECTLY') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('ALLOWED_CORRECTLY'));
    assert.ok(!output.includes('UNEXPECTED_DENY'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: the RAW composer turns the approval into a real selector — ↑/↓
// move the highlight and Enter confirms it, while 1/2/3 answer in one keypress.
// Bare Enter must still allow the focused option 1, because that is exactly
// what the daemon's AutoMode direct-fire writes to the PTY.
test('RAW approvals select with arrows plus Enter and answer on a single digit', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-selector-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId=null,turn=0;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-selector',configOptions:[]}});
else if(m.method==='session/prompt'){turn++;promptId=m.id;send({id:99,method:'session/request_permission',params:{sessionId:'session-selector',toolCall:{title:'Write a file',rawInput:{path:'test.txt'}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});}
else if(m.id===99){const ok=m.result.outcome.optionId==='allow';send({method:'session/update',params:{sessionId:'session-selector',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'T'+turn+'_'+(ok?'ALLOW':'DENY')}}}});send({id:promptId,result:{stopReason:'end_turn'}});promptId=null;}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, menuFrom = 0;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('write a file\r'); }
      else if (stage === 1 && output.includes('Select [1/')) { stage = 2; child.stdin.write('\x1b[B'); }
      // The arrow redraw surfaces the highlighted row in the composer status.
      else if (stage === 2 && output.includes('[2] No, reject')) { stage = 3; child.stdin.write('\r'); }
      else if (stage === 3 && output.includes('T1_DENY')) { stage = 4; menuFrom = output.length; child.stdin.write('go\r'); }
      else if (stage === 4 && output.slice(menuFrom).includes('[1] Yes, allow once')) { stage = 5; child.stdin.write('\r'); }
      else if (stage === 5 && output.includes('T2_ALLOW')) { stage = 6; menuFrom = output.length; child.stdin.write('go\r'); }
      else if (stage === 6 && output.slice(menuFrom).includes('[1] Yes, allow once')) { stage = 7; child.stdin.write('2'); }
      else if (stage === 7 && output.includes('T3_DENY')) { stage = 8; child.stdin.write('/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('T1_DENY'), `↓ + Enter did not select option 2\n${output.slice(-900)}`);
    assert.ok(output.includes('T2_ALLOW'), `bare Enter did not allow the focused option 1\n${output.slice(-900)}`);
    assert.ok(output.includes('T3_DENY'), `single '2' did not answer without Enter\n${output.slice(-900)}`);
    assert.ok(!output.includes('T1_ALLOW') && !output.includes('T2_DENY') && !output.includes('T3_ALLOW'), `wrong option answered\n${output.slice(-900)}`);
    assert.ok(!/enter to select/i.test(output), 'raw status leaked the generic-menu "enter to select" string');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('slow turns show a working indicator and a timed end-turn line', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-status-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-status',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;setTimeout(()=>{send({method:'session/update',params:{sessionId:'session-status',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'slow hello'}}}});send({id:promptId,result:{stopReason:'end_turn'}});},1300);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('hello\n'); }
      if (!exited && output.includes('slow hello') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('slow hello'));
    assert.match(output, /end_turn · \d+\.\ds/);
    assert.ok(errors.includes('working'));
    // Heartbeats must stay rare: one working line plus the done line, never a
    // per-tick frame per line (the \r-spinner leak this replaced).
    assert.ok(errors.split('\n').filter(line => line.includes('…')).length <= 2);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('turns are saved locally and /resume replays recent history', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-replay-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-replay',configOptions:[]}});
else if(m.method==='session/resume')send({id:m.id,result:{sessionId:m.params.sessionId,configOptions:[]}});
else if(m.method==='session/list')send({id:m.id,result:{sessions:[{sessionId:'session-replay',title:''},{sessionId:'old-session-aaaa',title:''}]}});
else if(m.method==='session/prompt'){promptId=m.id;send({method:'session/update',params:{sessionId:'session-replay',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'replayable hello'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    // An older session with its own transcript, predating the live one so it
    // sorts second in the most-recent-first list.
    await fs.mkdir(path.join(root, 'terminal-transcripts'), { recursive: true });
    await fs.writeFile(path.join(root, 'terminal-transcripts', 'old-session-aaaa.jsonl'),
      `${JSON.stringify({ t: 1000, role: 'user', text: 'earlier topic mars' })}\n${JSON.stringify({ t: 2000, role: 'agent', text: 'mars acknowledged', stop: 'end_turn' })}\n`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('remember this zebra\n'); }
      else if (stage === 1 && output.includes('replayable hello') && text.includes('▸ ')) { stage = 2; child.stdin.write('/resume\n'); }
      else if (stage === 2 && output.includes('Use /resume NUMBER or ID') && text.includes('▸ ')) { stage = 3; child.stdin.write('/resume 2\n'); }
      else if (stage === 3 && output.includes('earlier topic mars') && text.includes('▸ ')) { stage = 4; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('Sessions (most recent first):'));
    assert.match(output, /1\. [0-9a-z-]{8}…/);
    assert.match(output, /2\. [0-9a-z-]{8}…/);
    assert.ok(output.includes('earlier topic mars'));
    assert.ok(output.includes('Session old-session-aaaa resumed'));
    assert.ok(output.includes('replaying'));
    assert.ok(output.includes('Use /resume NUMBER or ID'));
    const saved = await fs.readFile(path.join(root, 'terminal-transcripts', 'session-replay.jsonl'), 'utf8');
    assert.ok(saved.includes('remember this zebra'));
    assert.ok(saved.includes('replayable hello'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('/import collapses tool runs so a tool-heavy turn cannot evict the conversation', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-replay-tools-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'replay-tools',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    // A realistic shape: two turns, the first of which made 30 tool calls. Before
    // the fix the last-20-raw-lines window was all bullets and both agent answers
    // were invisible on resume.
    const transcript = path.join(root, 'heavy.jsonl');
    const rows = [
      { t: 1, role: 'user', text: 'first question' },
      { t: 2, role: 'agent', text: 'first answer survives', stop: 'end_turn' },
      ...Array.from({ length: 30 }, (_, i) => ({ t: 3 + i, role: 'tool', kind: 'bash', title: `tool-title-${i}` })),
      { t: 40, role: 'user', text: 'second question' },
      { t: 41, role: 'agent', text: 'second answer survives', stop: 'end_turn' },
    ];
    await fs.writeFile(transcript, rows.map(row => JSON.stringify(row)).join('\n') + '\n');
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write(`/import ${transcript}\n`); }
      else if (stage === 1 && output.includes('── end replay ──') && output.includes('▸ ')) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('first answer survives'), output);
    assert.ok(output.includes('second answer survives'), output);
    assert.ok(output.includes('second question'), output);
    // The run is summarized, not enumerated one bullet per call.
    assert.ok(output.includes('30 tool calls'), output);
    assert.ok(!output.includes('tool-title-0'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

async function runImagePasteCase({ dirName, imageCapable, send, check }) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), dirName));
  try {
    const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');
    const shot = path.join(root, 'shot.png');
    await fs.writeFile(shot, png);
    const capture = path.join(root, 'captured.json');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:${imageCapable}}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-img',configOptions:[]}});
else if(m.method==='session/prompt'){fs.writeFileSync(process.env.CAPTURE_FILE,JSON.stringify(m.params.prompt));send({method:'session/update',params:{sessionId:'session-img',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'got it'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write(send(shot)); }
      if (!exited && output.includes('got it') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    const prompt = JSON.parse(await fs.readFile(capture, 'utf8'));
    check(prompt, output, png.toString('base64'), shot);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}

test('pasted @image is attached as an ACP image block when capable', async () => {
  await runImagePasteCase({
    dirName: 'solu-acp-imgcap-',
    imageCapable: true,
    send: shot => `@${shot} what is this\n`,
    check: (prompt, output, b64, shot) => {
      assert.equal(prompt.length, 2);
      assert.ok(prompt[0].text.includes('[image: shot.png]'));
      assert.ok(!prompt[0].text.includes('@'));
      assert.deepEqual(prompt[1], { type: 'image', data: b64, mimeType: 'image/png' });
    },
  });
});

test('pasted @image degrades to a plain path with guidance when incapable', async () => {
  await runImagePasteCase({
    dirName: 'solu-acp-imgno-',
    imageCapable: false,
    send: shot => `@${shot} hi\n`,
    check: (prompt, output, b64, shot) => {
      assert.equal(prompt.length, 1);
      assert.ok(prompt[0].text.includes(shot));
      assert.ok(!prompt[0].text.includes('@' + shot));
      assert.ok(output.includes("Images can't be attached here"));
    },
  });
});

test('renders reasoning, formatted markdown, tool calls and context usage', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-rich-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const tick=String.fromCharCode(96), fence=tick+tick+tick;
const text='## Result\\n\\nUse **bold** and '+tick+'inline'+tick+'.\\n\\n'+fence+'go\\nfunc main() { return }\\n'+fence;
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-rich',configOptions:[{id:'model',category:'model',currentValue:'deepseek-flash',options:[{value:'deepseek-flash',name:'DeepSeek Flash'}]},{id:'reasoning_effort',category:'reasoning_effort',currentValue:'high',options:[{value:'high',name:'high'}]}]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'session-rich',update:{sessionUpdate:'agent_thought_chunk',content:{type:'text',text:'weighing options'}}}});
send({method:'session/update',params:{sessionId:'session-rich',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text}}}});
send({method:'session/update',params:{sessionId:'session-rich',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'bash',kind:'other',status:'in_progress',rawInput:{command:'git status --short'}}}});
send({method:'session/update',params:{sessionId:'session-rich',update:{sessionUpdate:'usage_update',used:42000,size:100000}}});
send({method:'session/update',params:{sessionId:'session-rich',update:{sessionUpdate:'tool_call_update',toolCallId:'t1',status:'completed',content:[{type:'content',content:{type:'text',text:' M daemon/state.go'}}]}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      const prompts = (output.match(/▸ /g) || []).length;
      if (!sent && prompts >= 1) { sent = true; child.stdin.write('go\n'); }
      else if (sent && !exited && prompts >= 2) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // Reasoning is surfaced but marked as reasoning.
    assert.ok(output.includes('thinking'));
    assert.ok(output.includes('weighing options'));
    // Markdown: heading text kept, inline markers consumed, fenced code kept.
    assert.ok(output.includes('Result'));
    assert.ok(output.includes('bold') && !output.includes('**bold**'));
    assert.ok(output.includes('inline'));
    assert.ok(output.includes('func main() { return }'));
    // Tool lifecycle: the shell command and its result.
    assert.ok(output.includes('git status --short'));
    assert.ok(output.includes('M daemon/state.go'));
    // Context usage reported and carried into the input bar, with this turn's
    // growth (0 -> 42000) shown beside it.
    assert.ok(output.includes('ctx 42%'));
    assert.ok(output.includes('+42k'));
    // Model/effort surfaced from config options.
    assert.ok(output.includes('DeepSeek Flash'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('/tools collapse hides a finished tool body, which still reaches the model', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-tools-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'tools',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'tools',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'bash',kind:'other',status:'in_progress',rawInput:{command:'cat'}}}});
send({method:'session/update',params:{sessionId:'tools',update:{sessionUpdate:'tool_call_update',toolCallId:'t1',status:'completed',content:[{type:'content',content:{type:'text',text:'alpha-line\\nbravo-line\\ncharlie-line'}}]}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, started = false;
    const prompts = () => (output.match(/▸ /g) || []).length;
    const collapsedSeen = () => (output.match(/Tool output: collapsed/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!started && prompts() >= 1) { started = true; child.stdin.write('go\n'); }
      else if (started && stage === 0 && output.includes('charlie-line')) { stage = 1; child.stdin.write('/tools collapse\n'); }
      else if (stage === 1 && collapsedSeen() >= 1) { stage = 2; child.stdin.write('go\n'); }
      else if (stage === 2 && output.includes('more lines')) { stage = 3; child.stdin.write('/tools\n'); }
      else if (stage === 3 && collapsedSeen() >= 2) { stage = 4; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('Tool output: collapsed'), output);
    // The first turn rendered the full body; the collapsed turn rendered only the head.
    assert.equal((output.match(/bravo-line/g) || []).length, 1, output);
    assert.ok(output.includes('more lines'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('command surface: /help lists the registry and /thinking cycles modes', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-cmd-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-cmd',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('/help\n'); }
      else if (stage === 1 && output.includes('Type / to see suggestions')) { stage = 2; child.stdin.write('/thinking\n'); }
      else if (stage === 2 && output.includes('Reasoning display: full.')) { stage = 3; child.stdin.write('/thinking\n'); }
      else if (stage === 3 && output.includes('Reasoning display: off.')) { stage = 4; child.stdin.write('/clear\n'); }
      // /clear now starts a fresh session, so wait for the composer repaint
      // after it (a bare "Session" line is printed before submit releases).
      else if (stage === 4 && (output.match(/Session /g) || []).length >= 2 && prompts() >= 5) { stage = 5; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // /help is generated from the registry (names + descriptions).
    assert.ok(output.includes('start a fresh session'));
    assert.ok(output.includes('cycle reasoning display'));
    assert.ok(output.includes('Type / to see suggestions'));
    // /thinking cycles summary -> full -> off.
    assert.ok(output.includes('Reasoning display: full.'));
    assert.ok(output.includes('Reasoning display: off.'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('reports the build stamp the daemon writes beside the client', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-version-'));
  try {
    const client = path.join(root, 'deepseek-terminal.mjs');
    await fs.copyFile(fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), client);
    await fs.copyFile(fileURLToPath(new URL('./deepseek-model.mjs', import.meta.url)), path.join(root, 'deepseek-model.mjs'));
    await fs.writeFile(path.join(root, 'VERSION'), '0.0.1066\n');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-version',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    // Clear any ambient SOLU_CLIENT_BUILD (a SoluTerminal pane exports it) so
    // this test actually exercises the VERSION-file fallback it is asserting.
    const child = spawn(process.execPath, [client, runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_CLIENT_BUILD: '' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('/status\n'); }
      else if (stage === 1 && output.includes('client')) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // Banner and /status both surface the stamp, so a running pane can say
    // which client build it loaded.
    assert.ok(output.includes('SoluTerminal 0.0.1066'));
    assert.match(output, /client\s+0\.0\.1066/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('sweeps abandoned scratch dirs but keeps transcripts and history', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-scratch-'));
  try {
    const old = new Date(Date.now() - 12 * 60 * 60 * 1000);
    // An abandoned scratch dir from a SIGKILLed session (the cleanup never ran).
    const stale = path.join(root, 'terminal-ABCDEF');
    await fs.mkdir(stale, { recursive: true, mode: 0o700 });
    await fs.writeFile(path.join(stale, 'route.yaml'), '[]', { mode: 0o600 });
    await fs.utimes(stale, old, old);
    // Same mkdtemp shape but no route.yaml: not ours to delete.
    const foreign = path.join(root, 'terminal-GHIJKL');
    await fs.mkdir(foreign, { recursive: true, mode: 0o700 });
    await fs.utimes(foreign, old, old);
    // Long-lived siblings that only look similar; they must survive.
    const transcripts = path.join(root, 'terminal-transcripts');
    await fs.mkdir(transcripts, { recursive: true, mode: 0o700 });
    await fs.writeFile(path.join(transcripts, 'session.jsonl'), '{}\n', { mode: 0o600 });
    await fs.utimes(transcripts, old, old);
    const history = path.join(root, 'terminal-history');
    await fs.writeFile(history, '/status\n', { mode: 0o600 });
    await fs.utimes(history, old, old);

    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-scratch',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_CLIENT_BUILD: '9.9.9' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', exited = false;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!exited && output.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    await assert.rejects(fs.stat(stale), 'abandoned scratch dir should be swept');
    for (const keep of [foreign, transcripts, history, path.join(transcripts, 'session.jsonl')]) {
      await fs.stat(keep).catch(error => { throw new Error(`sweep removed ${keep}: ${error.message}`); });
    }
    // The current session's own scratch dir is cleaned up on /exit; only the
    // deliberately preserved foreign dir may remain.
    const leftovers = (await fs.readdir(root)).filter(name => /^terminal-[A-Za-z0-9]{6}$/.test(name) && name !== 'terminal-GHIJKL');
    assert.deepEqual(leftovers, [], `scratch dirs left behind: ${leftovers}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('does not inline @files named by untrusted !command output', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-shellref-'));
  try {
    await fs.writeFile(path.join(root, 'secret.txt'), 'CANARY_SECRET_42\n');
    const capture = path.join(root, 'prompt.json');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'shellref',configOptions:[]}});
else if(m.method==='session/prompt'){fs.writeFileSync(process.env.CAPTURE_FILE,JSON.stringify(m.params.prompt));send({method:'session/update',params:{sessionId:'shellref',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'ok'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture, SOLU_CLIENT_BUILD: '9.9.9' }, cwd: root, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('!echo @secret.txt\n'); }
      else if (stage === 1 && output.includes('ok') && prompts() >= 2) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 15000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    const prompt = JSON.parse(await fs.readFile(capture, 'utf8'));
    const text = prompt.map(part => part.text || '').join('\n');
    // The token is forwarded as literal text; its file is never read into the prompt.
    assert.ok(text.includes('@secret.txt'), `expected the literal token, got: ${text}`);
    assert.ok(!text.includes('CANARY_SECRET_42'), `command output triggered local file inlining: ${text}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a failed /resume keeps the session usable', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-resume-fail-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-fresh',configOptions:[]}});
else if(m.method==='session/resume')send({id:m.id,error:{code:-32000,message:'Internal error'}});
else if(m.method==='session/prompt'){
if(!m.params||!m.params.sessionId){send({id:m.id,error:{code:-32602,message:'Invalid params'}});return;}
send({method:'session/update',params:{sessionId:m.params.sessionId,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'RECOVERED_OK'}}}});
send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      // Wait on the composer repaint: the error line is written to stderr
      // before submit's finally runs, so a line sent on the error alone would
      // still see busy=true and be dropped.
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('/resume deadbeef-0000-0000-0000-000000000000\n'); }
      else if (stage === 1 && errors.includes('Could not resume') && prompts() >= 2) { stage = 2; child.stdin.write('hello\n'); }
      else if (stage === 2 && output.includes('RECOVERED_OK') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // The pane stays on its working session and the next turn still runs;
    // before the fix the resume closed the session first and the prompt went
    // out with no session id (Invalid params).
    assert.ok(errors.includes('Could not resume'));
    assert.ok(errors.includes('Staying on session'));
    assert.ok(output.includes('RECOVERED_OK'));
    assert.ok(!output.includes('Invalid params'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a failed /resume surfaces the server error detail, not just "Internal error"', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-resume-detail-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'session-live',configOptions:[]}});
else if(m.method==='session/resume')send({id:m.id,error:{code:-32603,message:'Internal error',data:{details:'session "locked" is already owned by another process'}}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('/resume locked\n'); }
      else if (stage === 1 && errors.includes('Could not resume') && prompts() >= 2) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(errors.includes('Could not resume'), errors);
    // The SDK parks the real cause in data.details; the client must surface it
    // instead of collapsing every server fault to "Internal error".
    assert.ok(errors.includes('already owned by another process'), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// A fake runtime that asks one permission per kind in `kinds`, in order, then
// ends the turn with each outcome ("allow"/"deny"/"cancel") in one line.
function permissionRuntime(kinds) {
  return `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;const kinds=${JSON.stringify(kinds)};const outcomes=[];
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const ask=i=>send({id:100+i,method:'session/request_permission',params:{sessionId:'s',toolCall:{title:'Tool '+kinds[i],kind:kinds[i],rawInput:{command:'rm -rf /tmp/x'}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;ask(0);}
else if(typeof m.id==='number'&&m.id>=100&&m.id<100+kinds.length){const o=m.result.outcome;outcomes.push(o.outcome==='selected'?o.optionId:'cancel');const next=m.id-100+1;
if(next<kinds.length)ask(next);else{send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'OUTCOMES:'+outcomes.join(',')}}}});send({id:promptId,result:{stopReason:'end_turn'}});}}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`;
}

// Drives one turn: answers each approval menu with the next key in `keys`,
// then runs /allow and exits. Returns the output and the menus drawn.
async function runPermissionTurn(root, kinds, keys) {
  const runtime = path.join(root, 'fake.mjs');
  await fs.writeFile(runtime, permissionRuntime(kinds));
  const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
  let output = '', errors = '', stage = 0, answered = 0;
  const prompts = () => (output.match(/▸ /g) || []).length;
  const menus = () => (output.match(/Tool approval:/g) || []).length;
  child.stdout.on('data', data => {
    output += data.toString();
    if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
    if (stage === 1) {
      while (answered < menus() && answered < keys.length) { child.stdin.write(`${keys[answered]}\n`); answered++; }
      if (output.includes('OUTCOMES:') && prompts() >= 2) { stage = 2; child.stdin.write('/allow\n'); }
    } else if (stage === 2 && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
  });
  child.stderr.on('data', data => { errors += data; });
  const timer = setTimeout(() => child.kill('SIGKILL'), 15000);
  const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
  assert.equal(code, 0, errors);
  return { output, menus: menus(), outcomes: (output.match(/OUTCOMES:([a-z,]*)/) || [])[1] };
}

test('no DeepSeek tool is offered Always allow; an unoffered 3 rejects and nothing is remembered', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-allow-'));
  try {
    const { output, menus, outcomes } = await runPermissionTurn(root, ['read', 'execute', 'edit', 'fetch', 'search'], ['3', '3', '3', '3', '3']);
    assert.ok(!output.includes('Always allow '));
    assert.ok(!output.includes('allowed automatically'));
    assert.equal(menus, 5);
    assert.equal(outcomes, 'deny,deny,deny,deny,deny');
    await assert.rejects(fs.readFile(path.join(root, 'terminal-allow.json'), 'utf8'));
    assert.ok(output.includes('No always-allow tools: DeepSeek tools ask each time.'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('an older allow file auto-answers nothing; its entries are dropped with a notice', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-allow-old-'));
  try {
    await fs.writeFile(path.join(root, 'terminal-allow.json'), JSON.stringify({ execute: 'Bash', edit: 'Edit', fetch: 'Web', read: 'Read' }));
    const { output, menus, outcomes } = await runPermissionTurn(root, ['execute', 'edit', 'fetch', 'read'], ['n', 'n', 'n', 'y']);
    assert.equal(menus, 4);
    assert.equal(outcomes, 'deny,deny,deny,allow');
    assert.ok(!output.includes('allowed automatically'));
    assert.ok(output.includes('Always allow is no longer available for DeepSeek tools; Bash, Edit, Web, Read will ask each time now.'));
    const saved = JSON.parse(await fs.readFile(path.join(root, 'terminal-allow.json'), 'utf8'));
    assert.deepEqual(saved, {});
    assert.ok(output.includes('No always-allow tools'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('an image tool result is saved to disk with its path shown', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-toolimg-'));
  try {
    const b64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
    // Pre-fill past the retention cap with old images; the run must prune them.
    const staleDir = path.join(root, 'attachments', 'tool-images');
    await fs.mkdir(staleDir, { recursive: true });
    for (let i = 0; i < 55; i++) {
      const p = path.join(staleDir, `image-old-${i}.png`);
      await fs.writeFile(p, 'x');
      const old = new Date('2001-01-01T00:00:00Z');
      await fs.utimes(p, old, old);
    }
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'Screenshot',kind:'read',status:'in_progress',rawInput:{path:'a.png'}}}});
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call_update',toolCallId:'t1',status:'completed',content:[{type:'content',content:{type:'image',mimeType:'image/png',data:'${b64}'}}]}}});
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'IMG_DONE'}}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && output.includes('IMG_DONE') && prompts() >= 2) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // The terminal cannot paint it, so the path is shown and the bytes land in
    // DSH_HOME/attachments/tool-images — with the old overflow pruned.
    const dir = path.join(root, 'attachments', 'tool-images');
    const files = await fs.readdir(dir);
    assert.ok(files.length <= 50, `${files.length} images left after pruning`);
    const saved = output.match(/image saved: (\S+)/);
    assert.ok(saved, output);
    assert.ok(files.includes(path.basename(saved[1])), saved[1]);
    const written = await fs.readFile(saved[1]);
    assert.equal(written.toString('base64'), b64);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('an unsettled tool call cannot outlive its turn', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-running-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // DSH settles a prompt only after its owned tools settle, so a tool left
    // 'in_progress' at turn end is a lost update. It must not keep the close
    // guard warning forever (the old code counted it and /exit refused to close).
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'Job output',kind:'other',status:'in_progress',rawInput:{}}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, afterStatus = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && output.includes('end_turn') && prompts() >= 2) { stage = 2; afterStatus = output.length; child.stdin.write('/status\n'); }
      else if (stage === 2 && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // The stale entry is gone, /status says none, and /exit closes without warning.
    const status = output.slice(afterStatus);
    assert.ok(status.includes('none'), status);
    assert.ok(!status.includes('1 running'), status);
    assert.ok(!output.includes('run /exit again'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('/clear resets the context readout like a fresh session', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-clear-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId,created=0;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new'){created+=1;send({id:m.id,result:{sessionId:'s'+created,configOptions:[]}});}
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'s'+created,update:{sessionUpdate:'usage_update',used:42000,size:100000}}});
send({method:'session/update',params:{sessionId:'s'+created,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'OK'}}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    const sessions = () => (output.match(/Session /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && output.includes('ctx 42%') && prompts() >= 2) { stage = 2; child.stdin.write('/status\n'); }
      else if (stage === 2 && output.includes('context') && prompts() >= 3) { stage = 3; child.stdin.write('/clear\n'); }
      else if (stage === 3 && sessions() >= 2 && prompts() >= 4) { stage = 4; child.stdin.write('/status\n'); }
      else if (stage === 4 && output.includes('unknown (harness has not reported usage)') && prompts() >= 5) { stage = 5; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // The old session reported 42%, /clear started a new one, and /status then
    // reports no usage yet — i.e. the composer ctx is no longer stale.
    assert.ok(output.includes('ctx 42%'));
    // /status also reports the session's accumulated context growth.
    assert.ok(output.includes('session +42k'), output);
    assert.ok(sessions() >= 2, output);
    assert.ok(output.includes('unknown (harness has not reported usage)'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a detached background job is tracked until job_output reports it done', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-bgjob-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // Turn 1 starts a background job (its spawning tool call completes at once,
    // which is why activeTools alone could never see it); turn 2 polls it done.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId,turn=0;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const result=(id,text)=>send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call_update',toolCallId:id,status:'completed',content:[{type:'content',content:{type:'text',text}}]}}});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;turn+=1;
if(turn===1){send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'Bash',kind:'execute',status:'in_progress',rawInput:{command:'publish',background:true}}}});result('t1','started background job bash-7');}
else {send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t2',title:'job_output',kind:'other',status:'in_progress',rawInput:{job_id:'bash-7',wait:true}}}});result('t2','[status: completed, exit code: 0]');}
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, afterComplete = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    const ends = () => (output.match(/end_turn/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && ends() >= 1 && prompts() >= 2) { stage = 2; child.stdin.write('/status\n'); }
      else if (stage === 2 && output.includes('bg: bash-7') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
      else if (stage === 3 && output.includes('run /exit again') && prompts() >= 4) { stage = 4; child.stdin.write('go\n'); }
      else if (stage === 4 && ends() >= 2 && prompts() >= 5) { stage = 5; afterComplete = output.length; child.stdin.write('/status\n'); }
      else if (stage === 5 && prompts() >= 6) { stage = 6; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    // Tracked from the result text, warned about on /exit, cleared by the poll.
    assert.ok(output.includes('started background job bash-7'));
    assert.ok(output.includes('bg: bash-7'));
    assert.ok(output.includes('run /exit again'));
    assert.ok(output.slice(afterComplete).includes('none'), output.slice(afterComplete));
    assert.ok(!output.slice(afterComplete).includes('bg:'), output.slice(afterComplete));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('job_list reconciles a background job that was never polled', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-joblist-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // Turn 1 starts a job; turn 2 lists it as finished without ever calling
    // job_output, which is the case that used to leave the badge stuck.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId,turn=0;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const call=(id,title,args)=>send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:id,title,kind:'other',status:'in_progress',rawInput:args}}});
const result=(id,text)=>send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call_update',toolCallId:id,status:'completed',content:[{type:'content',content:{type:'text',text}}]}}});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;turn+=1;
if(turn===1){call('t1','Bash',{command:'publish',background:true});result('t1','started background job bash-7');}
else {call('t2','job_list',{});result('t2','bash-7 [bash] completed — publish');}
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, afterList = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    const ends = () => (output.match(/end_turn/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && ends() >= 1 && prompts() >= 2) { stage = 2; child.stdin.write('/status\n'); }
      else if (stage === 2 && output.includes('bg: bash-7') && prompts() >= 3) { stage = 3; child.stdin.write('go\n'); }
      else if (stage === 3 && ends() >= 2 && prompts() >= 4) { stage = 4; afterList = output.length; child.stdin.write('/status\n'); }
      else if (stage === 4 && prompts() >= 5) { stage = 5; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('bg: bash-7'));
    assert.ok(output.slice(afterList).includes('none'), output.slice(afterList));
    assert.ok(!output.slice(afterList).includes('bg:'), output.slice(afterList));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a tool_call already reported completed is not counted as running', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-inline-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // Fast/instant tools can arrive as one settled tool_call with no follow-up
    // update. That used to sit in activeTools forever, so the close guard kept
    // warning about tasks that had already finished.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'Read',kind:'read',status:'completed',rawInput:{path:'a'},content:[{type:'content',content:{type:'text',text:'DONE_INLINE'}}]}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, afterStatus = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    const ends = () => (output.match(/end_turn/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('go\n'); }
      else if (stage === 1 && ends() >= 1 && prompts() >= 2) { stage = 2; afterStatus = output.length; child.stdin.write('/status\n'); }
      else if (stage === 2 && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    const status = output.slice(afterStatus);
    assert.ok(status.includes('tasks'), status);
    assert.ok(status.includes('none'), status);
    assert.ok(!status.includes('1 running'), status);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('client fails cleanly without DSH_HOME instead of crashing on path.join', async () => {
  const env = { ...process.env };
  delete env.DSH_HOME;
  const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), '/nonexistent/dsh.mjs', 'deepseek-flash'], { env, stdio: ['pipe', 'pipe', 'pipe'] });
  let errors = '';
  child.stderr.on('data', data => { errors += data; });
  const timer = setTimeout(() => child.kill('SIGKILL'), 8000);
  const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
  assert.equal(code, 1);
  assert.ok(errors.includes('DSH_HOME must be set'), errors);
  assert.ok(!errors.includes('TypeError'), errors);
});

test('an unanswered cancel unlocks the composer instead of hanging forever', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-hang-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // Answers initialize/new, then goes silent on the prompt and ignores
    // session/cancel — the "unrecoverable busy lock" case.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'hang',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_CANCEL_GRACE_MS: '250' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!sent && prompts() >= 1) {
        sent = true; child.stdin.write('do a long thing\n');
        // Let the client register the in-flight prompt, then cancel. The fake
        // never acknowledges, so only the watchdog can unlock the composer.
        setTimeout(() => child.kill('SIGINT'), 400);
      }
    });
    child.stderr.on('data', data => {
      errors += data.toString();
      // The unlock line is written just before the composer repaints; readline
      // buffers the line, so /exit can be sent immediately.
      if (!exited && errors.includes('input unlocked')) { exited = true; child.stdin.write('/exit\n'); }
    });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(errors.includes('input unlocked'), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a permission request with no params is answered, not a crash', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-noparams-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'noparams',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;send({id:99,method:'session/request_permission'});}
else if(m.id===99){send({method:'session/update',params:{sessionId:'noparams',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'PERMISSION_HANDLED'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, answered = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('act\n'); }
      if (!answered && output.includes('Select [1/2]: ')) { answered = true; child.stdin.write('n\n'); }
      if (!exited && output.includes('PERMISSION_HANDLED') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('PERMISSION_HANDLED'), output);
    assert.ok(output.includes('Tool approval:'), output);
    assert.ok(!errors.includes('TypeError'), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('/model lists and selects a config option', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-model-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const models=(selected)=>({id:'model',category:'model',currentValue:selected,options:[{value:'deepseek-flash',name:'One'},{value:'deepseek-v4-flash-vision-exp',name:'Two'}]});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'model',configOptions:[models('deepseek-flash')]}});
else if(m.method==='session/set_config_option')send({id:m.id,result:{configOptions:[models(m.params.value)]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('/model\n'); }
      else if (stage === 1 && output.includes('Use /model NUMBER') && prompts() >= 2) { stage = 2; child.stdin.write('/model 2\n'); }
      else if (stage === 2 && output.includes('Selected Two') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(output.includes('1. One (selected)'), output);
    assert.ok(output.includes('2. Two'), output);
    assert.ok(output.includes('Selected Two'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a stale live Pro selection is recovered to Flash before the prompt is dispatched', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-model-recovery-'));
  try {
    const capture = path.join(root, 'dispatch.json');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const events=[];
let selected='  DEEPSEEK-V4-PRO  ';
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const models=()=>({id:'model',category:'model',currentValue:selected,options:[{value:'deepseek-v4-pro',name:'Retired Pro'},{value:'deepseek-flash',name:'Flash'},{value:'deepseek-v4-flash-vision-exp',name:'Vision'}]});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'recovery',configOptions:[models()]}});
else if(m.method==='session/set_config_option'){selected=m.params.value;events.push('set:'+selected);send({id:m.id,result:{configOptions:[models()]}});}
else if(m.method==='session/prompt'){events.push('prompt:'+selected);fs.writeFileSync(process.env.CAPTURE_FILE,JSON.stringify(events));send({method:'session/update',params:{sessionId:'recovery',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'MODEL_RECOVERED'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'provider/deepseek-v4-pro'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('hello\n'); }
      if (!exited && output.includes('MODEL_RECOVERED') && text.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.deepEqual(JSON.parse(await fs.readFile(capture, 'utf8')), ['set:deepseek-flash', 'prompt:deepseek-flash']);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the API key never reaches the terminal from harness stderr or output', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-redact-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
process.stderr.write('leak=' + (process.env.DEEPSEEK_API_KEY || '') + '\\n');
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'redact',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'sk-redact-sentinel' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    assert.ok(errors.includes('leak=[redacted]'), errors);
    assert.ok(!errors.includes('sk-redact-sentinel'), errors);
    assert.ok(!output.includes('sk-redact-sentinel'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('image capability is not reused after /model switches route', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-imgmodel-'));
  try {
    const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');
    const shot = path.join(root, 'shot.png');
    await fs.writeFile(shot, png);
    const capture = path.join(root, 'captured.jsonl');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const models=v=>({id:'model',category:'model',currentValue:v,options:[{value:'deepseek-flash',name:'One'},{value:'deepseek-v4-flash-vision-exp',name:'Two'}]});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:true}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'imgmodel',configOptions:[models('deepseek-flash')]}});
else if(m.method==='session/set_config_option')send({id:m.id,result:{configOptions:[models(m.params.value)]}});
else if(m.method==='session/prompt'){fs.appendFileSync(process.env.CAPTURE_FILE,JSON.stringify(m.params.prompt)+'\\n');send({method:'session/update',params:{sessionId:'imgmodel',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'ack'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('/model 2\n'); }
      else if (stage === 1 && output.includes('Selected Two') && prompts() >= 2) { stage = 2; child.stdin.write(`@${shot} hi\n`); }
      else if (stage === 2 && output.includes('✓ end_turn') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, errors);
    const sent = (await fs.readFile(capture, 'utf8')).trim().split('\n').map(line => JSON.parse(line));
    assert.equal(sent.length, 1, JSON.stringify(sent));
    assert.equal(sent[0].length, 1, JSON.stringify(sent[0]));
    assert.ok(sent[0][0].text.includes(shot));
    assert.ok(output.includes('does not accept image input'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('an oversized ACP frame closes the client instead of buffering forever', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-oversize-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // A frame with no newline larger than the bound: the client must refuse it.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
input.on('line',()=>{ process.stdout.write('x'.repeat(5000)); });
input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_WIRE_MAX_LINE: '1000' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let errors = '';
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 1, errors);
    assert.ok(errors.includes('oversized frame'), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('two outstanding permission requests are queued, not auto-cancelled', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-queue-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;const outcomes=[];
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const done=()=>{ if(outcomes.length<2)return; send({method:'session/update',params:{sessionId:'q',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'QUEUED:'+outcomes.join(',')}}}}); send({id:promptId,result:{stopReason:'end_turn'}}); };
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'q',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
  send({id:99,method:'session/request_permission',params:{sessionId:'q',toolCall:{title:'First tool',rawInput:{}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});
  send({id:100,method:'session/request_permission',params:{sessionId:'q',toolCall:{title:'Second tool',rawInput:{}},options:[{optionId:'allow',kind:'allow_once'},{optionId:'deny',kind:'reject_once'}]}});}
else if(m.id===99||m.id===100){outcomes.push(m.result.outcome.optionId||m.result.outcome.outcome);done();}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', menus = 0, exited = false, sent = false;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      const text = data.toString(); output += text;
      if (!sent && prompts() >= 1) { sent = true; child.stdin.write('go\n'); }
      const count = (output.match(/Select \[1\/2\]: /g) || []).length;
      if (count > menus) { const first = menus === 0; menus = count; child.stdin.write(first ? '1\n' : '2\n'); }
      if (!exited && output.includes('QUEUED:') && prompts() >= 2) { exited = true; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('First tool'), output);
    assert.ok(output.includes('Second tool'), output);
    assert.match(output, /QUEUED:allow,deny/, output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a background job announced by a settled tool_call is tracked and listed by /tasks', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-inline-bg-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    // Fast tools arrive as one settled tool_call carrying their content, with no
    // follow-up update. That is where a real background-job start lands, so the
    // client must process the inline result, not only tool_call_update.
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'s',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'s',update:{sessionUpdate:'tool_call',toolCallId:'t1',title:'Bash',kind:'execute',status:'completed',rawInput:{command:'publish',background:true},content:[{type:'content',content:{type:'text',text:'started background job bash-9'}}]}}});
send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('publish\n'); }
      else if (stage === 1 && output.includes('✓ end_turn') && prompts() >= 2) { stage = 2; child.stdin.write('/tasks\n'); }
      else if (stage === 2 && output.includes('Background tasks') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
      else if (stage === 3 && output.includes('run /exit again')) { stage = 4; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('started background job bash-9'), output);
    assert.ok(output.includes('Background tasks'), output);
    assert.match(output, /bash-9 · running · \d+s/, output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the launcher build stamp overrides the local VERSION file', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-envbuild-'));
  try {
    const client = path.join(root, 'deepseek-terminal.mjs');
    await fs.copyFile(fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), client);
    await fs.copyFile(fileURLToPath(new URL('./deepseek-model.mjs', import.meta.url)), path.join(root, 'deepseek-model.mjs'));
    // A stale local stamp must not win over the launcher's resolved build.
    await fs.writeFile(path.join(root, 'VERSION'), '0.0.1\n');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'envbuild',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [client, runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_CLIENT_BUILD: '9.9.9' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', exited = false;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!exited && output.includes('▸ ')) { exited = true; child.stdin.write('/exit\n'); }
    });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise(resolve => child.on('exit', resolve));
    clearTimeout(timer);
    assert.equal(code, 0, output);
    assert.ok(output.includes('SoluTerminal 9.9.9'), output);
    assert.ok(!output.includes('SoluTerminal 0.0.1'), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('a message typed while a turn runs is queued and sent as the next prompt', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-steer-'));
  try {
    const capture = path.join(root, 'prompts.jsonl');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});let n=0;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'steer',configOptions:[]}});
else if(m.method==='session/prompt'){n+=1;const text=String((m.params.prompt[0]||{}).text||'');fs.appendFileSync(process.env.CAPTURE_FILE,text+'\\n');const id=m.id;
setTimeout(()=>{send({method:'session/update',params:{sessionId:'steer',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'ack:'+text}}}});send({id,result:{stopReason:'end_turn'}});},n===1?300:20);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) {
        stage = 1;
        child.stdin.write('first message\n');
        // Land while the first turn is still running (its reply is 300ms away).
        setTimeout(() => child.stdin.write('second message\n'), 60);
      } else if (stage === 1 && output.includes('ack:second message') && prompts() >= 2) {
        stage = 2; child.stdin.write('/exit\n');
      }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise(resolve => child.on('exit', resolve));
    clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('⇢ queued (1): second message'), output);
    assert.ok(output.includes('ack:second message'), output);
    const sent = (await fs.readFile(capture, 'utf8')).trim().split('\n');
    assert.deepEqual(sent, ['first message', 'second message']);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('/hotkeys lists bindings, /export copies the transcript, and /import replays it', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-hotkeys-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'hotkeys',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;send({method:'session/update',params:{sessionId:'hotkeys',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'done'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key' }, stdio: ['pipe', 'pipe', 'pipe'] });
    const exportPath = path.join(root, 'session-export.jsonl');
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('remember the zebra\n'); }
      else if (stage === 1 && output.includes('done') && prompts() >= 2) { stage = 2; child.stdin.write('/hotkeys\n'); }
      else if (stage === 2 && output.includes('Key bindings') && prompts() >= 3) { stage = 3; child.stdin.write(`/export ${exportPath}\n`); }
      else if (stage === 3 && output.includes('Exported this session to') && prompts() >= 4) { stage = 4; child.stdin.write(`/import ${exportPath}\n`); }
      else if (stage === 4 && output.includes('replaying last') && prompts() >= 5) { stage = 5; child.stdin.write('/import /no/such/transcript.jsonl\n'); }
      else if (stage === 5 && output.includes('Import failed') && prompts() >= 6) { stage = 6; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise(resolve => child.on('exit', resolve));
    clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('Key bindings'), output);
    assert.ok(output.includes('queue a steering message'), output);
    assert.ok(output.includes(`Exported this session to ${exportPath}`), output);
    assert.ok(output.includes('── replaying last'), output);
    assert.ok(output.includes('remember the zebra'), output);
    assert.ok(output.includes('Import failed'), output);
    const exported = await fs.readFile(exportPath, 'utf8');
    assert.ok(exported.includes('remember the zebra'), exported);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('!command runs locally and sends its output; !!command does not', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-bang-'));
  try {
    const capture = path.join(root, 'prompts.jsonl');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'bang',configOptions:[]}});
else if(m.method==='session/prompt'){fs.appendFileSync(process.env.CAPTURE_FILE,JSON.stringify(m.params.prompt)+'\\n');send({method:'session/update',params:{sessionId:'bang',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'ack'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('!echo hello-from-shell\n'); }
      else if (stage === 1 && output.includes('ack') && prompts() >= 2) { stage = 2; child.stdin.write('!!echo quiet-marker\n'); }
      else if (stage === 2 && output.includes('quiet-marker') && output.includes('[exit 0]') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise(resolve => child.on('exit', resolve));
    clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('hello-from-shell'), output);
    assert.ok(output.includes('quiet-marker'), output);
    const sent = (await fs.readFile(capture, 'utf8')).trim().split('\n').map(line => JSON.parse(line));
    assert.equal(sent.length, 1, JSON.stringify(sent));
    assert.ok(JSON.stringify(sent[0]).includes('hello-from-shell'));
    assert.ok(!JSON.stringify(sent[0]).includes('quiet-marker'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('@file references are inlined into the prompt', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-atfile-'));
  try {
    await fs.writeFile(path.join(root, 'notes.txt'), 'zebra content 123\n');
    const capture = path.join(root, 'prompt.json');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'atfile',configOptions:[]}});
else if(m.method==='session/prompt'){fs.writeFileSync(process.env.CAPTURE_FILE,JSON.stringify(m.params.prompt));send({method:'session/update',params:{sessionId:'atfile',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'ok'}}}});send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, cwd: root, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('@notes.txt summarize\n'); }
      else if (stage === 1 && output.includes('ok') && prompts() >= 2) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise(resolve => child.on('exit', resolve));
    clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('Inlined notes.txt'), output);
    const prompt = JSON.parse(await fs.readFile(capture, 'utf8'));
    const text = prompt.map(part => part.text || '').join('\n');
    assert.ok(text.includes('zebra content 123'), text);
    assert.ok(text.includes('--- notes.txt ---'), text);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: output that arrives AFTER a turn settled used to be written at the
// composer's parked input cell, overwriting its input/status rows — the "after a
// reply the text input area is cut off / gone" report. The composer must be
// erased before the write and repainted after it. Forced RAW lets this run over
// pipes; the normal suite takes the readline branch and never covers it.
test('a late update after a turn repaints the composer instead of overwriting it', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-late-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'late',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
send({method:'session/update',params:{sessionId:'late',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'done'}}}});
send({id:promptId,result:{stopReason:'end_turn'}});
setTimeout(()=>send({method:'session/update',params:{sessionId:'late',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'LATE_UPDATE_MARKER'}}}}),150);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, settled = '';
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('go\r'); }
      else if (stage === 1 && output.includes('LATE_UPDATE_MARKER')) {
        stage = 2;
        // Snapshot once the redraw has had a tick to land, then exit. /exit
        // itself repaints the composer, so it must not be in the snapshot.
        setTimeout(() => { settled = output; child.stdin.write('/exit\r'); }, 80);
      }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(settled.includes('LATE_UPDATE_MARKER'), `late update missing\n${output.slice(-600)}`);
    // The composer (prefix + rule + status) must be redrawn AFTER the late text.
    assert.ok(settled.lastIndexOf('▸ ') > settled.lastIndexOf('LATE_UPDATE_MARKER'), `composer not repainted after late output\n${settled.slice(-600)}`);
    assert.ok(settled.slice(settled.lastIndexOf('LATE_UPDATE_MARKER')).includes('Ctrl+J newline'), `status row not repainted\n${settled.slice(-600)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// The composer is the single always-on bottom UI now, so a draft typed during a
// turn stays on the input row (not in a separate live line) and the busy status
// row advertises that Enter queues it. Forced RAW covers the path.
test('a draft typed during a turn stays on the always-on composer', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-draft-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'draft',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
setTimeout(()=>{send({method:'session/update',params:{sessionId:'draft',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'working'}}}});send({id:promptId,result:{stopReason:'end_turn'}});},350);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', started = false;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!started && output.includes('▸ ')) {
        started = true;
        child.stdin.write('go\r');
        // Type without Enter while the turn is still running (the fake holds it
        // for 350ms), then exit after the turn has settled.
        setTimeout(() => { try { child.stdin.write('draft text'); } catch {} }, 150);
        // The draft is deliberately kept after the turn; clear the line (Ctrl+U)
        // before /exit, or the two concatenate into a new prompt.
        setTimeout(() => { try { child.stdin.write('\x15/exit\r'); } catch {} }, 1500);
      }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('▸ draft text'), `draft not shown on the composer input row\n${output.slice(-800)}`);
    assert.ok(output.includes('type to queue'), `busy composer did not advertise queuing\n${output.slice(-800)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// A submitted prompt is echoed back as an unmistakable user block, not a bare
// arrow line that reads like more assistant output. Forced RAW covers the
// composer echo path; the color-off shape is the stable assertion target.
test('a submitted prompt is echoed as a labeled user block', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-userecho-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'userecho',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;setTimeout(()=>{send({id:promptId,result:{stopReason:'end_turn'}});},150);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', sent = false, exited = false;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!sent && output.includes('▸ ')) { sent = true; child.stdin.write('what is the plan?\r'); }
      else if (sent && !exited && output.includes('▸ you') && output.includes('│ what is the plan?')) {
        exited = true;
        setTimeout(() => { try { child.stdin.write('\x15/exit\r'); } catch {} }, 120);
      }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('▸ you\n│ what is the plan?'), `user turn not rendered as a labeled block\n${output.slice(-800)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: a resize mid-turn used to leave the 8fps live line drawing at a
// stale, too-wide width, so every frame wrapped past the single-row erase and
// leaked as a permanent transcript line (one "running …" line per tick). The
// live line must keep re-measuring the pane while busy, and a SIGWINCH
// mid-turn must keep the turn alive.
test('a resize mid-turn keeps the live line measuring the pane', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-live-resize-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});let promptId;
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'resize',configOptions:[]}});
else if(m.method==='session/prompt'){promptId=m.id;
setTimeout(()=>{send({id:promptId,result:{stopReason:'end_turn'}});},2200);}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', started = false, exited = false;
    let turnStart = -1, turnEnd = -1;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!started && output.includes('▸ ')) {
        started = true;
        turnStart = output.length;
        child.stdin.write('go\r');
        setTimeout(() => { try { child.kill('SIGWINCH'); } catch {} }, 500);
      }
      if (turnEnd < 0 && output.includes('end_turn')) turnEnd = output.length;
      if (!exited && turnEnd >= 0) { exited = true; child.stdin.write('\x15/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(turnEnd >= 0, `turn did not survive the mid-turn resize\n${output.slice(-800)}`);
    // Count width probes strictly inside the turn window: the SIGWINCH probe
    // plus at least one mid-turn probe from the 1.5s watch. Without mid-turn
    // re-measurement a resize during a long turn is never healed. The client
    // uses the non-moving CSI 18 t report, so the probe bytes are the query —
    // there is no caret jump to count.
    const inTurn = output.slice(turnStart, turnEnd);
    const probes = inTurn.split('\x1b[18t').length - 1;
    assert.ok(probes >= 2, `expected a mid-turn width probe, saw ${probes}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: the idle width watch used to fire every 1.5 s. The probe moves
// the caret, and xterm keeps the viewport on the caret, so a reader who had
// scrolled up was yanked back to the bottom a second or two later. An idle
// client must not emit width probes on a timer; while busy it still must.
test('idle does not re-probe the width on a timer', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-idle-probe-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'idle',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', started = false, idleFrom = -1;
    child.stdout.on('data', data => {
      output += data.toString();
      if (!started && output.includes('▸ ')) {
        started = true;
        // Let the boot/prompt probes land, then count only what the idle
        // client emits on its own over the next few seconds.
        setTimeout(() => { idleFrom = output.length; }, 300);
      }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => { try { child.stdin.write('\x15/exit\r'); } catch {} }, 5000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(idleFrom >= 0, `client never reached the idle composer\n${output.slice(-800)}`);
    const probes = output.slice(idleFrom).split('\x1b[18t').length - 1;
    assert.equal(probes, 0, `idle client emitted ${probes} width probe(s) — an idle tick yanks a scrolled-up reader to the bottom`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// The width probe must not move the caret. The old probe walked the cursor to
// the right margin (ESC[9999C) and read its position back, which flashed the
// caret in the right margin on every keystroke and every mid-turn tick — the
// "random flashing typing indicator" report. The client now asks with the
// non-moving text-area report (CSI 18 t), and only a real TTY that never
// answers it may fall back to the caret-moving probe.
test('typing re-measures the width without moving the caret', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-probe-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'probe',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('hello'); }
      else if (stage === 1 && output.includes('▸ hello')) { stage = 2; child.stdin.write('\x15/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => { try { child.stdin.write('\x15/exit\r'); } catch {} }, 8000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(stage >= 1, `client never reached the idle composer\n${output.slice(-800)}`);
    assert.ok(output.includes('\x1b[18t'), 'client did not use the non-moving text-area report');
    assert.ok(!output.includes('\x1b[9999C'), 'client walked the caret to the right margin (the flashing-indicator bug)');
    assert.ok(output.includes('Type a message'), 'idle composer did not show the input placeholder');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// /status reads the daemon's imported ledger directly (no round-trip) and shows
// the selected account's spend.
test('/status shows the imported spend for the selected account', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-spend-'));
  try {
    await fs.mkdir(path.join(root, 'harness-accounts'), { recursive: true });
    await fs.writeFile(path.join(root, 'harness-accounts', 'deepseek-usage.json'), JSON.stringify({
      version: 1,
      imports: {
        'amount-2026-09.csv': {
          source: 'amount-2026-09.csv', importedAt: 1, rowCount: 5, currency: 'CNY', costTotal: 6.08,
          keys: { main: { requests: 42, cacheHitInputTokens: 1, cacheMissInputTokens: 1, outputTokens: 1, cost: 6.08 } },
        },
      },
    }));
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'spend',configOptions:[]}});
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1', SOLU_HARNESS_DATA_DIR: root, SOLU_HARNESS_ACCOUNT: 'main' }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('/status\r'); }
      else if (stage === 1 && output.includes('imported')) { stage = 2; child.stdin.write('/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    assert.ok(output.includes('CNY 6.08 imported'), `spend not shown in /status\n${output.slice(-700)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// The daemon-side resume contract for a managed harness pane: the client
// records its DSH session id against SOLU_PANE_ID, and a later launch on the
// same pane resumes it. Mirrors Claude/Codex, where the durable conversation
// id survives the pane agent being replaced.
test('a pane remembers its DSH session and resumes it on the next launch', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-pane-resume-'));
  try {
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
let promptId;
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'persisted-session-1',configOptions:[]}});
else if(m.method==='session/resume'){if(m.params.sessionId==='gone-session')send({id:m.id,error:{code:-32000,message:'session not found'}});else send({id:m.id,result:{sessionId:m.params.sessionId,configOptions:[]}});}
else if(m.method==='session/prompt'){promptId=m.id;send({method:'session/update',params:{sessionId:'persisted-session-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'READY'}}}});send({id:promptId,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const baseEnv = { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_PANE_ID: 'pane-resume-1', SOLU_HARNESS_DATA_DIR: root };
    const client = fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url));

    const runOnce = async (env, waitFor) => {
      const child = spawn(process.execPath, [client, runtime, 'deepseek-flash'], { env, stdio: ['pipe', 'pipe', 'pipe'] });
      let out = '', err = '', done = false;
      child.stdout.on('data', data => {
        out += data.toString();
        if (!done && out.includes(waitFor)) { done = true; setTimeout(() => { try { child.stdin.write('/exit\n'); } catch {} }, 100); }
      });
      child.stderr.on('data', data => { err += data; });
      const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
      const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); });
      clearTimeout(timer);
      return { code, out, err };
    };

    // First launch: fresh session, sidecar recorded before the prompt returns.
    const first = await runOnce(baseEnv, '▸ ');
    assert.equal(first.code, 0, `first run errors=${first.err}\nOUTPUT:\n${first.out}`);
    const sidecar = JSON.parse(await fs.readFile(path.join(root, 'pane-sessions', 'pane-resume-1.json'), 'utf8'));
    assert.equal(sidecar.provider, 'deepseek');
    assert.equal(sidecar.sessionId, 'persisted-session-1');

    // Second launch on the same pane resumes the recorded session.
    const second = await runOnce(baseEnv, 'resumed');
    assert.equal(second.code, 0, `second run errors=${second.err}\nOUTPUT:\n${second.out}`);
    assert.ok(second.out.includes('persisted-session-1 resumed'), `pane did not resume its recorded session\n${second.out.slice(-500)}`);

    // The daemon's explicit hint wins over the sidecar.
    const third = await runOnce({ ...baseEnv, SOLU_HARNESS_RESUME_SESSION: 'env-session-9' }, 'resumed');
    assert.equal(third.code, 0, `third run errors=${third.err}\nOUTPUT:\n${third.out}`);
    assert.ok(third.out.includes('env-session-9 resumed'), `daemon resume hint ignored\n${third.out.slice(-500)}`);

    // A stale sidecar must fall back to a fresh session, never brick the client.
    await fs.writeFile(path.join(root, 'pane-sessions', 'pane-resume-1.json'), JSON.stringify({ provider: 'deepseek', sessionId: 'gone-session' }));
    const fourth = await runOnce({ ...baseEnv, SOLU_HARNESS_RESUME_SESSION: 'gone-session' }, 'Session ');
    assert.equal(fourth.code, 0, `fourth run errors=${fourth.err}\nOUTPUT:\n${fourth.out}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: Ctrl+Shift+V in a SoluTerminal pane goes through xterm's native
// paste path, which only wraps the paste in bracketed-paste markers when the
// foreground app opts in with DEC 2004. The composer never opted in, so every
// newline in a pasted chunk arrived as a bare \r: the first line submitted
// immediately and rawData's `return` dropped everything after it.
test('a multi-line paste lands whole and submits once on Enter', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-paste-'));
  try {
    const capture = path.join(root, 'prompts.txt');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'paste',configOptions:[]}});
else if(m.method==='session/prompt'){const id=m.id;const text=(m.params.prompt||[]).map(p=>p.text||'').join('');fs.appendFileSync(process.env.CAPTURE_FILE,text+'\\n---\\n');send({method:'session/update',params:{sessionId:'paste',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'TURN_DONE'}}}});send({id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0, earlySubmit = false;
    const turns = () => (output.match(/end_turn/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && output.includes('▸ ')) {
        stage = 1;
        // What xterm sends for a two-line Ctrl+Shift+V paste once DEC 2004 is
        // on: newlines arrive as \r inside the markers, never as submit keys.
        child.stdin.write('\x1b[200~chunk of pasted words\r\nsecond line here\x1b[201~');
        setTimeout(() => {
          try { if (fsSync.existsSync(capture)) earlySubmit = true; } catch {}
          try { child.stdin.write('\r'); } catch {}
        }, 400);
      }
      // The footer prints after the turn settles (busy=false), so /exit can
      // never land mid-turn and be queued as a steering message.
      else if (stage === 1 && output.includes('TURN_DONE') && turns() >= 1) { stage = 2; child.stdin.write('/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    // The composer must opt into bracketed paste, or real panes keep sending
    // pastes unbracketed (xterm only brackets when DEC 2004 is set).
    assert.ok(output.includes('\x1b[?2004h'), `composer did not enable bracketed paste\n${output.slice(-400)}`);
    assert.ok(!earlySubmit, 'the pasted newlines submitted before Enter was pressed');
    const prompts = await fs.readFile(capture, 'utf8');
    assert.equal(prompts, 'chunk of pasted words\nsecond line here\n---\n', `paste was not submitted whole:\n${JSON.stringify(prompts)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Regression: rawData abandoned the rest of its input buffer on Enter
// (`rawSubmit(); return`), so a paste whose trailing bytes shared the same
// read as an Enter lost them. Bytes after \r must survive as type-ahead.
test('bytes after Enter in the same read are kept, not dropped', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-tail-'));
  try {
    const capture = path.join(root, 'prompts.txt');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'tail',configOptions:[]}});
else if(m.method==='session/prompt'){const id=m.id;const text=(m.params.prompt||[]).map(p=>p.text||'').join('');fs.appendFileSync(process.env.CAPTURE_FILE,text+'\\n---\\n');send({method:'session/update',params:{sessionId:'tail',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'TURN_DONE:'+text.slice(0,20)}}}});send({id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', SOLU_HARNESS_FORCE_RAW: '1', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    // The footer prints after the turn settles (busy=false), so the next \r
    // can never land mid-turn and be queued as a steering message instead.
    const turns = () => (output.match(/end_turn/g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      // One atomic write: a single pipe read carries the Enter AND its tail,
      // exactly like a paste burst coalesced with a keystroke.
      if (stage === 0 && output.includes('▸ ')) { stage = 1; child.stdin.write('hello\rworld'); }
      else if (stage === 1 && output.includes('TURN_DONE:hello') && turns() >= 1) { stage = 2; child.stdin.write('\r'); }
      else if (stage === 2 && output.includes('TURN_DONE:world') && turns() >= 2) { stage = 3; child.stdin.write('/exit\r'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `errors=${errors}\nOUTPUT:\n${output}`);
    const saved = await fs.readFile(capture, 'utf8');
    assert.equal(saved, 'hello\n---\nworld\n---\n', `tail after Enter was lost:\n${JSON.stringify(saved)}`);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// Against the real pinned DeepSeek Harness (gated: it needs the managed install
// and Node 24.9+, which CI's Node 20 lacks). A local fake Messages server
// records every request: the pane client's patch must keep the session log and
// the plugin list out of all of them.
test('the pinned DeepSeek Harness sends no session log or plugin list from a pane', async t => {
  const { runtimeRoot, nodeSupportsDsh } = await import('./launch.mjs');
  const { PINS } = await import('./pins.mjs');
  const runtime = path.join(runtimeRoot, 'deepseek/node_modules/@deepseek-ai/dsh/lib/bin.js');
  const version = await fs.readFile(path.join(path.dirname(runtime), '..', 'package.json'), 'utf8').then(raw => JSON.parse(raw).version, () => '');
  if (!nodeSupportsDsh() || version !== PINS.deepseek) return t.skip(`needs DeepSeek Harness ${PINS.deepseek} installed and Node 24.9+`);
  const { createServer } = await import('node:http');
  const seen = [];
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', data => { body += data; });
    req.on('end', () => {
      let keys = [];
      try { keys = Object.keys(JSON.parse(body)); } catch {}
      seen.push({ url: req.url, keys, telemetry: /dsh_session_log|dsh_plugin_packages/.test(body) });
      if (!req.url.endsWith('/v1/messages')) { res.writeHead(404, { 'content-type': 'application/json' }); res.end('{"type":"error","error":{"type":"not_found_error","message":"no"}}'); return; }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const ev = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
      ev('message_start', { message: { id: 'msg_1', type: 'message', role: 'assistant', model: 'deepseek-flash', content: [], stop_reason: null, usage: { input_tokens: 3, output_tokens: 0 } } });
      ev('content_block_start', { index: 0, content_block: { type: 'text', text: '' } });
      ev('content_block_delta', { index: 0, delta: { type: 'text_delta', text: 'pong' } });
      ev('content_block_stop', { index: 0 });
      ev('message_delta', { delta: { stop_reason: 'end_turn' }, usage: { output_tokens: 1 } });
      ev('message_stop', {});
      res.end();
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-privacy-'));
  try {
    const env = { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', DEEPSEEK_BASE_URL: `http://127.0.0.1:${server.address().port}/anthropic` };
    delete env.DSH_TELEMETRY_DISABLED; delete env.DSH_TELEMETRY_MODE;
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('say pong\n'); }
      else if (stage === 1 && output.includes('✓ end_turn') && prompts() >= 2) { stage = 2; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 90000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `${output}\n${errors}`);
    assert.ok(output.includes('pong'), output);
    const messages = seen.filter(row => row.url.endsWith('/anthropic/v1/messages'));
    assert.ok(messages.length >= 1, JSON.stringify(seen));
    for (const row of seen) {
      assert.ok(!row.keys.includes('dsh_session_log') && !row.keys.includes('dsh_plugin_packages') && !row.telemetry, JSON.stringify(row));
    }
  } finally {
    server.close();
    await fs.rm(root, { recursive: true, force: true });
  }
});

test('0.2.2 model pairs decode to the official model and refuse other providers', async () => {
  const { decodeDshModelValue, encodeDshModelValue } = await import('./deepseek-model.mjs');
  assert.deepEqual(decodeDshModelValue('["deepseek-official","deepseek-flash"]'), { model: 'deepseek-flash', pair: true });
  assert.deepEqual(decodeDshModelValue('deepseek-flash'), { model: 'deepseek-flash', pair: false });
  assert.equal(encodeDshModelValue('deepseek-flash', true), '["deepseek-official","deepseek-flash"]');
  assert.equal(encodeDshModelValue('deepseek-flash', false), 'deepseek-flash');
  for (const bad of ['["openai","gpt-5"]', '["deepseek-official"]', '["deepseek-official",1]', '[not json']) {
    assert.throws(() => decodeDshModelValue(bad), /Unsupported DeepSeek model/, bad);
  }
});

test('a stale 0.2.2 model pair is recovered to the Flash pair before the prompt', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-acp-pair-'));
  try {
    const capture = path.join(root, 'captured.jsonl');
    const runtime = path.join(root, 'fake.mjs');
    await fs.writeFile(runtime, `import readline from 'node:readline';
import fs from 'node:fs';
const input=readline.createInterface({input:process.stdin});
const send=x=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',...x})+'\\n');
const pair=m=>JSON.stringify(['deepseek-official',m]);
const models=v=>({id:'model',category:'model',currentValue:v,options:[{group:'deepseek-official',name:'DeepSeek',options:[{value:pair('deepseek-v4-flash'),name:'deepseek-v4-flash'},{value:pair('deepseek-flash'),name:'DeepSeek-V4.1-Flash'},{value:pair('deepseek-v4-pro'),name:'DeepSeek-V4-Pro'}]}]});
input.on('line',line=>{const m=JSON.parse(line);
if(m.method==='initialize')send({id:m.id,result:{protocolVersion:1,agentCapabilities:{promptCapabilities:{image:false}}}});
else if(m.method==='session/new')send({id:m.id,result:{sessionId:'pair',configOptions:[models(pair('deepseek-v4-flash'))]}});
else if(m.method==='session/set_config_option'){fs.appendFileSync(process.env.CAPTURE_FILE,JSON.stringify({set:m.params.value})+'\\n');send({id:m.id,result:{configOptions:[models(m.params.value)]}});}
else if(m.method==='session/prompt'){fs.appendFileSync(process.env.CAPTURE_FILE,JSON.stringify({prompt:true})+'\\n');send({id:m.id,result:{stopReason:'end_turn'}});}
else if(m.method==='session/close')send({id:m.id,result:{}});
});input.on('close',()=>process.exit(0));`);
    const child = spawn(process.execPath, [fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url)), runtime, 'deepseek-flash'], { env: { ...process.env, DSH_HOME: root, DEEPSEEK_API_KEY: 'synthetic-key', CAPTURE_FILE: capture }, stdio: ['pipe', 'pipe', 'pipe'] });
    let output = '', errors = '', stage = 0;
    const prompts = () => (output.match(/▸ /g) || []).length;
    child.stdout.on('data', data => {
      output += data.toString();
      if (stage === 0 && prompts() >= 1) { stage = 1; child.stdin.write('/model\n'); }
      else if (stage === 1 && output.includes('Use /model NUMBER') && prompts() >= 2) { stage = 2; child.stdin.write('hi\n'); }
      else if (stage === 2 && output.includes('✓ end_turn') && prompts() >= 3) { stage = 3; child.stdin.write('/exit\n'); }
    });
    child.stderr.on('data', data => { errors += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.equal(code, 0, `${output}\n${errors}`);
    const sent = (await fs.readFile(capture, 'utf8')).trim().split('\n').map(line => JSON.parse(line));
    assert.deepEqual(sent, [{ set: '["deepseek-official","deepseek-flash"]' }, { prompt: true }]);
    // Only the current Flash id is offered: not the retired v4-flash, not Pro.
    assert.ok(output.includes('1. DeepSeek-V4.1-Flash'), output);
    assert.ok(!/\d\. deepseek-v4-flash|\d\. DeepSeek-V4-Pro/.test(output), output);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
