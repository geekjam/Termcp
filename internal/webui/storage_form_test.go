package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStorageFormPasswordLifecycleAndMigrationNotices(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise storage form")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "storage.js")
	if err := os.WriteFile(source, []byte(readAssetLF(t, "static/storage.js")), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe.js")
	script := `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const source = fs.readFileSync(process.argv[2], 'utf8');
const drain = async () => { for (let i=0;i<5;i++) await new Promise(setImmediate); };
function harness(state) {
  const elements = {}, calls = [], events = {}, timers = [], redirects = [];
  let reply = { ok:true, value:{state:'RECOVERING',password_configured:true,progress:{}} };
  function element(id) {
    return elements[id] || (elements[id] = { value:'', hidden:false, textContent:'', listeners:{}, addEventListener(event,callback){this.listeners[event]=callback;} });
  }
  const sandbox = {
    document:{getElementById:element}, storedLangChoice:()=> 'en', chooseLang:()=>{},
    t:key=>key, onLangChange:()=>{}, setInterval:callback=>timers.push(callback),
    window:{addEventListener:(event,callback)=>events[event]=callback,location:{replace:path=>redirects.push(path)}},
    localStorage:{setItem(){throw Error('password persisted in localStorage');}},
    sessionStorage:{setItem(){throw Error('password persisted in sessionStorage');}},
    fetch:(path,options)=> {
      calls.push({path,options});
      const value = options.method === 'POST' ? reply : {ok:true,value:state};
      return Promise.resolve({ok:value.ok,json:()=>Promise.resolve(value.value)});
    }
  };
  vm.runInNewContext(source,sandbox);
  return {element,calls,events,timers,redirects,setState:value=>state=value,setReply:value=>reply=value,
    submit:()=>element('storage-form').listeners.submit({preventDefault(){}}),
    changeMode:value=>{element('storage-unlock-mode').value=value;element('storage-unlock-mode').listeners.change();},
    submitMode:()=>element('storage-mode-form').listeners.submit({preventDefault(){}}),
    posts:()=>calls.filter(call=>call.options.method==='POST')};
}
(async()=>{
  let h = harness({state:'UNINITIALIZED',progress:{},legacy_found:true}); await drain();
  assert.equal(h.element('storage-form').hidden,false);
  assert.equal(h.element('storage-current-row').hidden,true);
  assert.equal(h.element('storage-migration').hidden,false);
  h.element('storage-new').value='placeholder first'; h.element('storage-confirm').value='placeholder different';
  h.submit(); await drain(); assert.equal(h.posts().length,0);
  assert.equal(h.element('storage-error').textContent,'storage.error.passwordConfirmationMismatch');
  h.element('storage-confirm').value='placeholder first';
  h.setState({state:'RECOVERING',progress:{},password_configured:true});
  h.submit(); h.submit(); await drain(); assert.equal(h.posts().length,1);
  assert.equal(h.posts()[0].path,'api/storage/initialize');
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{new_password:'placeholder first',confirm_password:'placeholder first'});
  assert.equal(h.element('storage-new').value,''); assert.equal(h.element('storage-confirm').value,'');
  assert.equal(h.redirects.length,0);
  h.setState({state:'MIGRATING',legacy_found:true,progress:{stage:'archive',resuming:true,total:5,completed:2,bytes_total:1000,bytes_completed:500}});
  await h.timers[0](); await drain(); assert.equal(h.element('storage-resuming').hidden,false);
  assert(h.element('storage-progress').textContent.includes('2 / 5'));
  h.setState({state:'CLEANUP_PENDING',password_configured:true,progress:{}});
  await h.timers[0](); await drain(); assert.equal(h.element('storage-cleanup').hidden,false);
  assert.equal(h.redirects.length,0); assert.equal(h.element('storage-workspace').hidden,false);
  h.setState({state:'READY',password_configured:true,progress:{}});
  await h.timers[0](); await drain(); assert.deepEqual(h.redirects,['./']);

  h=harness({state:'LOCKED',password_configured:true,progress:{}}); await drain();
  assert.equal(h.element('storage-new-row').hidden,true);
  h.element('storage-current').value='placeholder current'; h.submit(); await drain();
  assert.equal(h.posts()[0].path,'api/storage/unlock');
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{password:'placeholder current'});

  h=harness({state:'READY',password_configured:true,progress:{}}); await drain();
  assert.equal(h.element('storage-current').required,true);
  h.element('storage-current').value='placeholder current';
  h.element('storage-new').value=h.element('storage-confirm').value='placeholder replacement';
  h.setReply({ok:false,value:{error:'invalid_current_password'}}); h.submit(); await drain();
  assert.equal(h.posts()[0].path,'api/storage/password');
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{current_password:'placeholder current',new_password:'placeholder replacement',confirm_password:'placeholder replacement'});
  assert.equal(h.element('storage-error').textContent,'storage.error.invalidCurrentPassword');
  h.events.pagehide();
  for(const id of ['storage-current','storage-new','storage-confirm']) assert.equal(h.element(id).value,'');

  h=harness({state:'READY',auto_unlock:true,password_configured:false,progress:{}}); await drain();
  assert.equal(h.element('storage-form').hidden,false);
  assert.equal(h.element('storage-current-row').hidden,true);
  assert.equal(h.element('storage-current').required,false);
  assert.equal(h.element('storage-state').textContent,'storage.state.readyManaged');
  assert.equal(h.element('storage-auto-unlock').textContent,'storage.autoUnlock.enabled');
  h.element('storage-new').value=h.element('storage-confirm').value='placeholder optional';
  h.setReply({ok:true,value:{state:'READY',auto_unlock:true,password_configured:true,progress:{}}});
  h.setState({state:'READY',auto_unlock:true,password_configured:true,progress:{}});
  h.submit(); await drain();
  assert.equal(h.posts()[0].path,'api/storage/initialize');
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{new_password:'placeholder optional',confirm_password:'placeholder optional'});
  assert.equal(h.element('storage-current-row').hidden,false);

  h=harness({state:'LOCKED',password_configured:true,progress:{},error_code:'system_unlock_failed'}); await drain();
  h.element('storage-current').value='placeholder recovery';
  const recovered={state:'READY',password_configured:true,auto_unlock:false,auto_unlock_error:'automatic_unlock_unavailable',progress:{}};
  h.setReply({ok:true,value:recovered}); h.setState(recovered);
  h.submit(); await drain();
  assert.equal(h.element('storage-auto-unlock').textContent,'storage.autoUnlock.unavailable');
  assert.equal(h.element('storage-error').hidden,true);
  assert.equal(h.redirects.length,0);

  h=harness({state:'READY',unlock_mode:'automatic',unlock_mode_saved:false,auto_unlock:true,password_configured:false,progress:{}}); await drain();
  assert.equal(h.element('storage-unlock-mode').value,'automatic');
  h.changeMode('password');
  assert.equal(h.element('storage-mode-new-row').hidden,false);
  assert.equal(h.element('storage-mode-current-row').hidden,true);
  assert.equal(h.element('storage-mode-new').required,true);
  assert.equal(h.element('storage-form').hidden,true);
  await h.timers[0](); await drain();
  assert.equal(h.element('storage-unlock-mode').value,'password'); // polling preserves an unsaved choice
  h.element('storage-mode-new').value='placeholder manual'; h.element('storage-mode-confirm').value='mismatch';
  h.submitMode(); await drain(); assert.equal(h.posts().length,0);
  h.element('storage-mode-confirm').value='placeholder manual';
  const manual={state:'READY',unlock_mode:'password',unlock_mode_saved:true,auto_unlock:false,password_configured:true,progress:{}};
  h.setReply({ok:true,value:manual}); h.setState(manual);
  h.submitMode(); h.submitMode(); await drain();
  assert.equal(h.posts().length,1);
  assert.equal(h.posts()[0].path,'api/storage/unlock-mode');
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{unlock_mode:'password',new_password:'placeholder manual',confirm_password:'placeholder manual'});
  assert.equal(h.element('storage-unlock-mode').value,'password');
  assert.equal(h.element('storage-mode-new').value,'');
  assert.equal(h.element('storage-mode-save').disabled,true);
  assert.equal(h.element('storage-result').textContent,'storage.mode.saved');

  h=harness(manual); await drain(); h.changeMode('automatic');
  assert.equal(h.element('storage-mode-current-row').hidden,false);
  assert.equal(h.element('storage-mode-current').required,true);
  assert.equal(h.element('storage-mode-new-row').hidden,true);
  h.element('storage-mode-current').value='placeholder manual';
  h.setReply({ok:false,value:{error:'automatic_unlock_unavailable'}}); h.submitMode(); await drain();
  assert.deepEqual(JSON.parse(h.posts()[0].options.body),{unlock_mode:'automatic',current_password:'placeholder manual'});
  assert.equal(h.element('storage-error').textContent,'storage.error.automaticUnlockUnavailable');
  assert.equal(h.element('storage-unlock-mode').value,'automatic'); // failed selection stays editable
  h.events.pagehide(); assert.equal(h.element('storage-mode-current').value,'');

  h=harness({state:'LOCKED',unlock_mode:'password',unlock_mode_saved:true,password_configured:true,progress:{}}); await drain();
  assert.equal(h.element('storage-unlock-mode').value,'password');
  assert.equal(h.element('storage-unlock-mode').disabled,true);
  assert.equal(h.element('storage-mode-save').disabled,true);
})().catch(error=>{console.error(error);process.exit(1);});
`
	if err := os.WriteFile(probe, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, probe, source).CombinedOutput(); err != nil {
		t.Fatalf("storage form probe: %v\n%s", err, output)
	}
}
