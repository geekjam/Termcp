package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestConnectionTransferFormOptionalPasswordsAndCleanup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise transfer forms")
	}
	source := between(t, readAssetLF(t, "static/js/conn-form.js"), "var connImportBusy = false;", "// One request for every id")
	dir := t.TempDir()
	src := filepath.Join(dir, "transfer.js")
	probe := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
const fs=require('fs'),vm=require('vm'),assert=require('assert');
const source=fs.readFileSync(process.argv[2],'utf8');
const elements={},calls=[],downloads=[],events={},toasts=[];
let response={ok:true,json:{imported:1}},selected=['one'];
function el(id){return elements[id]||(elements[id]={value:'',style:{},checked:false,disabled:false,files:[],textContent:''});}
const sandbox={
  document:{getElementById:el,body:{appendChild(){}},createElement(){return{click(){downloads.push(this.download);},remove(){}};}},
  window:{addEventListener:(event,fn)=>events[event]=fn,_lastConnections:[]},
  nodeSelectModeOn:()=>true,selectableNodeNames:()=>selected,_nodeSelIds:new Set(['one']),showCopyToast:value=>toasts.push(value),
  showModal:id=>el(id).hidden=false,hideModal:id=>el(id).hidden=true,loadConnections(){},
  t:key=>key,apiPath:path=>'/mounted'+path,setTimeout:fn=>fn(),
  URL:{createObjectURL:()=> 'blob:test',revokeObjectURL(){}},
  FormData:class{constructor(){this.parts=[];}append(name,value){this.parts.push([name,value]);}},
  localStorage:{setItem(){throw Error('password persisted');}},sessionStorage:{setItem(){throw Error('password persisted');}},
  fetch:(path,options)=>{calls.push({path,options});const r=response;return Promise.resolve({ok:r.ok,json:()=>Promise.resolve(r.json),blob:()=>Promise.resolve({})});}
};
vm.createContext(sandbox);vm.runInContext(source,sandbox);
async function drain(){for(let i=0;i<6;i++)await new Promise(setImmediate);}
function click(id){el(id).onclick.call(el(id),{stopPropagation(){}});}
(async()=>{
  sandbox.exportSelectedNodes();assert.equal(el('modal-conn-export').hidden,false);assert.equal(calls.length,0);
  click('conn-export-run');click('conn-export-run');await drain();
  assert.equal(calls.length,1);assert.equal(calls[0].path,'/mounted/api/connections/batch/export');
  assert.deepEqual(JSON.parse(calls[0].options.body),{names:['one'],password:'',confirm_password:''});
  assert.equal(downloads[0],'termcp-connections.toml');
  sandbox.openConnectionExport(['one']);
  el('conn-export-password').value='placeholder export';el('conn-export-confirm').value='different placeholder';
  click('conn-export-run');await drain();assert.equal(calls.length,1);
  assert.equal(el('conn-export-err').textContent,'conn.batch.passwordMismatch');
  el('conn-export-confirm').value='placeholder export';click('conn-export-run');
  assert.equal(el('conn-export-password').value,'');assert.equal(el('conn-export-confirm').value,'');await drain();
  assert.equal(downloads[1],'termcp-connections.toml.enc');
  assert.equal(JSON.parse(calls[1].options.body).password,'placeholder export');
  assert(!calls[1].path.includes('placeholder'));
  sandbox.openConnectionExport(['one']);el('conn-export-password').value='placeholder cancelled';click('conn-export-close');
  assert.equal(el('conn-export-password').value,'');assert.equal(el('modal-conn-export').hidden,true);
  sandbox.window._lastConnections=[{name:'one',temporary:true}];sandbox.exportSelectedNodes();assert.equal(toasts.pop(),'nethub.batch.exportTemporary');

  click('conn-import-open');el('conn-import-file').files=[{name:'configuration.enc'}];
  el('conn-import-password').value='placeholder import';el('conn-import-temporary').checked=true;
  click('conn-import-run');click('conn-import-run');assert.equal(el('conn-import-password').value,'');await drain();
  assert.equal(calls.length,3);assert.equal(calls[2].path,'/mounted/api/connections/batch/import?temporary=true');
  assert.equal(calls[2].options.headers,undefined); // browser sets the multipart boundary
  assert.equal(calls[2].options.body.parts.find(part=>part[0]==='password')[1],'placeholder import');
  assert.equal(el('conn-import-run').disabled,false);
  response={ok:false,json:{error:'invalid_import_password_or_file'}};
  click('conn-import-open');el('conn-import-file').files=[{name:'configuration.enc'}];el('conn-import-password').value='wrong placeholder';
  click('conn-import-run');await drain();assert.equal(el('conn-import-err').textContent,'conn.batch.passwordInvalid');
  el('conn-import-password').value=el('conn-export-password').value=el('conn-export-confirm').value='placeholder pending';events.pagehide();
  for(const id of ['conn-import-password','conn-export-password','conn-export-confirm'])assert.equal(el(id).value,'');
})().catch(error=>{console.error(error);process.exit(1);});
`
	if err := os.WriteFile(probe, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, probe, src).CombinedOutput(); err != nil {
		t.Fatalf("transfer form probe: %v\n%s", err, output)
	}
}
