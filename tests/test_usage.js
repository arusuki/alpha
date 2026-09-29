'use strict';
const assert = require('assert');
const fs = require('fs');
const Usage = require('../dist/usage.js');
const node = (path, bytes, children=[], extra={}) => ({path,name:path.split('/').pop(),kind:'directory',allocated:bytes,apparent:bytes,files:0,errors:0,children,...extra});
const container = (id, owner, upper_path, mounts=[]) => ({id,name:id,owner,upper_path,mounts,state:'running',writable_layer:{allocated:upper_path?0:null,apparent:upper_path?0:null,status:upper_path?'complete':'unavailable',permission_denied:false}});
const mount = source => ({source,type:'bind',destination:source,rw:true});
const resource = (path, containers, kinds=['bind']) => ({path,containers,kinds});
const data = (children, containers, resources=[]) => ({
  tree:node('@root',children.reduce((sum,n)=>sum+n.allocated,0),children,{kind:'root'}), containers,
  scan:{omitted_references:0},
  filesystems:[],
  resources:[...resources,...containers.flatMap(c=>[
    ...(c.upper_path?[resource(c.upper_path,[c.id],['writable'])]:[]),
    ...c.mounts.filter(m=>['bind','volume'].includes(m.type)&&m.source).map(m=>resource(m.source,[c.id],[m.type])),
    ...(c.log_path?[resource(c.log_path.slice(0,c.log_path.lastIndexOf('/')),[c.id],['container-data'])]:[])
  ])]
});
const conserved = (d,m) => assert.equal(m.exclusive+m.shared+m.unrelated,d.tree.allocated,'known allocated bytes must be conserved');
const demo = JSON.parse(fs.readFileSync('tests/fixtures/snapshot.json','utf8'));
let result = Usage.build(demo);
const gib = 1024**3;
assert(Math.abs(result.owners.get('yuuka').bytes-96*gib)<=2);
assert(Math.abs(result.owners.get('lin').bytes-48*gib)<=2);
assert.equal(result.shared,696*gib);
assert.equal(result.crossOwner,696*gib);
assert.equal(result.unrelated,8*gib);
conserved(demo,result);
// Host-root binds (read-only or writable) convey access, not disk ownership.
for (const rw of [false,true]) {
  const s = JSON.parse(JSON.stringify(demo));
  const baseline = Usage.build(s), c = s.containers[0];
  c.mounts.push({type:'bind',source:'/',destination:'/run/host',rw});
  s.resources.push(resource('/',[c.id],['host','bind']));
  const actual = Usage.build(s);
  for (const field of ['exclusive','shared','unrelated','crossOwner']) assert.equal(actual[field],baseline[field]);
  for (const [id,row] of actual.containers) for (const field of ['exclusive','shared','known','partial']) assert.equal(row[field],baseline.containers.get(id)[field]);
  const source = Usage.sources(s,actual,c).find(s=>s.destination==='/run/host');
  assert(source.accessOnly);assert(source.hint.includes('不计入容器用量'));
  conserved(s,actual);
}
// Parent/child mounts: only their intersection is shared, including collapsed bytes.
// Partition entrances are discovered from the snapshot, regardless of path or RW.
for (const entrance of ['/boot/efi','/data','/mnt/arbitrary partition']) for (const rw of [false,true]) {
  const directory=entrance+'/app-data', other=entrance+'-other';
  const s=data([node(entrance,100,[node(directory,30)]),node(other,20),node('/upper',10)],
    [container('monitor','ops','/upper'),container('app','alice',null,[mount(directory),mount(other)])]);
  s.filesystems=[{mount:entrance,fs:'ext4'}];
  const baseline=Usage.build(s), monitor=s.containers[0];
  monitor.mounts.push({...mount(entrance),rw});
  s.resources.push(resource(entrance,['monitor']));
  const actual=Usage.build(s);
  for(const field of ['exclusive','shared','unrelated','crossOwner']) assert.equal(actual[field],baseline[field]);
  assert.equal(actual.containers.get('monitor').exclusive,10);
  assert.equal(actual.containers.get('app').exclusive,50);
  assert.equal(actual.unrelated,70);
  assert.deepEqual(actual.owners,baseline.owners);
  assert(Usage.sources(s,actual,monitor).find(source=>source.path===entrance).accessOnly);
  assert(Usage.sources(s,actual,s.containers[1]).filter(source=>source.type==='bind').every(source=>!source.accessOnly));
  conserved(s,actual);
  // Removing a mount-table entry makes this an ordinary bind again.
  s.filesystems=[];
  assert.equal(Usage.build(s).containers.get('monitor').exclusive,80);
  // Named volumes at the same path retain their ownership semantics.
  s.filesystems=[{mount:entrance,fs:'ext4'}];
  s.resources[s.resources.length-1].kinds=['volume'];
  monitor.mounts[0].type='volume';
  assert.equal(Usage.build(s).containers.get('monitor').exclusive,80);
  assert(!Usage.sources(s,Usage.build(s),monitor).find(source=>source.path===entrance).accessOnly);
}
let fixture = data([node('/data',100,[node('/data/models',60)])],[container('a','alice','/data'),container('b','bob','/data/models')]);
result=Usage.build(fixture);
assert.equal(result.containers.get('a').exclusive,40);
assert.equal(result.containers.get('a').shared,60);
assert.equal(result.containers.get('b').exclusive,0);
assert.equal(result.owners.get('alice').bytes,40);
assert.equal(result.crossOwner,60);
conserved(fixture,result);
// The same user's shared source is charged once, never twice.
fixture.containers[1].owner='alice'; result=Usage.build(fixture);
assert.equal(result.owners.get('alice').bytes,100);
assert.equal(result.crossOwner,0);
// Duplicate mount destinations and duplicate resource rows do not increase usage.
fixture=data([node('/models',80)],[container('a','alice',null,[mount('/models'),mount('/models')]),container('b','bob',null,[mount('/models')])],[resource('/models',['a','b']),resource('/models',['a'])]);
result=Usage.build(fixture);
assert.equal(result.shared,80);assert.equal(result.containers.get('a').shared,80);
assert(result.containers.get('a').partial);conserved(fixture,result);
// A hard link is shared even when only its first path carries allocated bytes.
fixture=data([node('/a',40,[node('/a/file',40)]),node('/b',0,[node('/b/file',0,[],{kind:'reference',reference:'/a/file'})])],[container('a','alice','/a'),container('b','bob','/b')]);
result=Usage.build(fixture);
assert.equal(result.shared,40);assert.equal(result.crossOwner,40);assert.equal(result.exclusive,0);
// Directory alias propagation must also follow hard links inside its target.
fixture=data([node('/a',40,[node('/a/own',10),node('/a/link',0,[],{kind:'reference',reference:'/c/file'})]),node('/b',0,[],{kind:'reference',reference:'/a'}),node('/c',20,[node('/c/file',20)])],[container('a','alice','/a'),container('b','bob','/b'),container('c','chen','/c')]);
result=Usage.build(fixture);
assert.equal(result.containers.get('b').shared,60);
assert.equal(result.shared,60);conserved(fixture,result);
// Unassigned containers must not be treated as one known person.
fixture=data([node('/shared',80)],[container('a','未标注','/shared'),container('b','  ','/shared')]);
result=Usage.build(fixture);assert.equal(result.owners.get('').bytes,0);assert.equal(result.crossOwner,80);
fixture.containers.pop();result=Usage.build(fixture);assert.equal(result.owners.get('').bytes,80);
// Logs and exclusive mounts contribute to the container, host-only files do not.
fixture=data([node('/upper',10),node('/logs/a',20),node('/volume',30),node('/host',50)],[{...container('a','alice','/upper',[mount('/volume')]),log_path:'/logs/a/a-json.log'}],[resource('/logs/a',['a'],['container-data'])]);
result=Usage.build(fixture);assert.equal(result.owners.get('alice').bytes,60);assert.equal(result.unrelated,50);conserved(fixture,result);
// Missing physical data is not replaced with Docker SizeRw.
fixture=data([], [{...container('a','alice',null),size_rw:123456}]);
result=Usage.build(fixture);assert(!result.containers.get('a').known);assert(result.containers.get('a').partial);
// An excluded descendant makes its container and owner partial, even with no errors.
fixture=data([node('/upper',10,[node('/upper/excluded',0,[],{kind:'excluded'})])],[container('a','alice','/upper')]);
result=Usage.build(fixture);assert(result.owners.get('alice').partial);assert.equal(result.owners.get('alice').bytes,10);
fixture.tree.children[0]=node('/upper',0,[],{kind:'unreadable',errors:1});result=Usage.build(fixture);assert(!result.owners.get('alice').known);
fixture=data([node('/upper',10,[],{omitted_entries:3})],[container('a','alice','/upper')]);
result=Usage.build(fixture);assert(!result.attributionLimited);assert(!result.containers.get('a').partial);
fixture.scan.omitted_references=1;result=Usage.build(fixture);assert(result.attributionLimited);
// Folded exclusions make the containing layer partial.
fixture=data([node('/upper',10,[],{excluded_entries:1,omitted_entries:4})],[container('a','alice','/upper')]);
fixture.containers[0].writable_layer={allocated:10,apparent:10,status:'partial',permission_denied:false};result=Usage.build(fixture);
assert(result.containers.get('a').partial);assert.equal(result.containers.get('a').writable.status,'partial');
// An explicit unreadable layer never becomes zero or gets charged at SizeRw.
fixture.containers[0].writable_layer={allocated:null,apparent:null,status:'unreadable',reason:'permission denied',permission_denied:true};
fixture.containers[0].size_rw=1024**4;result=Usage.build(fixture);
assert.equal(result.containers.get('a').writable.allocated,null);assert(!result.containers.get('a').writable.known);
assert.equal(result.exclusive,10);conserved(fixture,result);
console.log('Usage checks passed: shared and nested mounts, owner de-duplication, inode references, logs, unassigned users, unknown/partial values and byte conservation.');

// Explorer areas must conserve bytes, including directory metadata and omitted data.
fixture=data([node('/upper',100,[node('/upper/large',60),node('/upper/empty',0),node('/upper/denied',0,[],{kind:'unreadable',errors:1}),node('/upper/alias',0,[],{kind:'reference',reference:'/upper/large'})],{omitted_entries:3})],[container('a','alice','/upper')]);
result=Usage.build(fixture);
let entries=Usage.directoryEntries(result,result.nodes.get('/upper'));
assert.equal(entries.reduce((sum,e)=>sum+(e.bytes||0),0),100);
assert.equal(entries.find(e=>e.kind==='residual').bytes,40);
assert.equal(entries.find(e=>e.kind==='unreadable').bytes,null);
assert.equal(entries.find(e=>e.name==='empty').bytes,0);
assert.equal(entries.find(e=>e.kind==='reference').bytes,0);
assert.equal(entries.find(e=>e.kind==='reference').node.path,'/upper/large');
let tiles=Usage.treemap(Usage.mapEntries(entries));
assert.equal(tiles.length,2);
assert(Math.abs(tiles.reduce((sum,t)=>sum+t.w*t.h,0)-6000)<1e-7);
for (const tile of tiles) assert(Math.abs(tile.w*tile.h/6000-tile.bytes/100)<1e-7);
// Compacted physical paths remain readable relative to the current directory.
fixture.tree.children[0].children[0].path='/upper/opt/cache/large';
result=Usage.build(fixture);
assert.equal(Usage.directoryEntries(result,result.nodes.get('/upper'))[0].name,'opt/cache/large');
// Grouped map tiles preserve the total size of all entries.
entries=Array.from({length:501},(_,index)=>({name:'file-'+index,bytes:501-index,kind:'file'}));
const mapped=Usage.mapEntries(entries), total=entries.reduce((sum,e)=>sum+e.bytes,0);
assert.equal(mapped.length,48);assert.equal(entries.length,501);
assert.equal(mapped.reduce((sum,e)=>sum+e.bytes,0),total);
assert.equal(mapped[47].kind,'group');assert.equal(mapped[47].index,-1);
tiles=Usage.treemap(mapped);
for(let i=0;i<tiles.length;i++) {
  const a=tiles[i];
  assert(a.x>=0 && a.y>=0 && a.w>0 && a.h>0 && a.x+a.w<=100+1e-7 && a.y+a.h<=60+1e-7);
  assert(Math.abs(a.w*a.h/6000-a.bytes/total)<1e-7);
  for(const b of tiles.slice(i+1)) assert(Math.min(a.x+a.w,b.x+b.w)-Math.max(a.x,b.x)<1e-7 || Math.min(a.y+a.h,b.y+b.h)-Math.max(a.y,b.y)<1e-7,'tiles must not overlap');
}
assert.deepEqual(Usage.treemap([{bytes:0}]),[]);
// Unknown, mounted, aliased and log sources stay separate and retain their paths.
fixture=data([node('/volume',30),node('/logs/a',20),node('/alias',0,[],{kind:'reference',reference:'/volume'})],[{...container('a','alice',null,[{...mount('/alias'),destination:'/data'},{type:'tmpfs',source:'/tmp',destination:'/tmp',rw:true}]),log_path:'/logs/a/a.log'}],[resource('/logs/a',['a'],['container-data'])]);
result=Usage.build(fixture);
const sources=Usage.sources(fixture,result,fixture.containers[0]);
assert.equal(sources.length,4);assert(!sources[0].known);
assert.equal(sources[1].destination,'/data');assert.equal(sources[1].node.allocated,30);
assert.equal(sources[1].path,'/alias');assert.equal(sources[2].path,null);
assert.equal(sources[3].path,'/logs/a');
fixture=data([node('/config.json',42,[],{kind:'file'})],[container('a','alice',null,[mount('/config.json')])]);
result=Usage.build(fixture);
entries=Usage.directoryEntries(result,result.nodes.get('/config.json'));
assert.equal(entries.length,1);assert.equal(entries[0].kind,'file');assert.equal(entries[0].bytes,42);
console.log('Storage explorer checks passed: residual conservation, unknown/zero/reference distinctions, relative paths, bounded proportional treemaps and source navigation.');

// Host maps partition mixed physical directories using the same alias-aware
// claims as the overview; container mounts and hard-linked data are removed.
fixture=data([node('/',200,[
  node('/srv',140,[node('/srv/host',60,[node('/srv/host/file',20,[],{kind:'file'}),node('/srv/host/cache',30,[],{omitted_entries:4})]),node('/srv/mounted',70)]),
  node('/var',50,[node('/var/upper',30),node('/var/log/container',10)]),
  node('/denied',0,[],{kind:'unreadable',errors:1}),node('/excluded',0,[],{kind:'excluded'}),
  node('/unknown',0,[],{size_unknown:true}),node('/empty',0)
])],[{...container('a','alice','/var/upper',[mount('/srv/mounted')]),log_path:'/var/log/container/log.json'}]);
result=Usage.build(fixture);
assert.equal(result.unrelated,90);
assert.equal(result.host.get('@root').allocated,90);
assert.equal(result.host.get('/srv').allocated,70);
assert.equal(result.host.get('/var').allocated,10);
assert(!result.host.get('/srv/mounted').visible);
entries=Usage.directoryEntries(result,result.nodes.get('/srv'),true);
assert(!entries.some(e=>e.name==='mounted'));
assert.equal(entries.find(e=>e.kind==='residual').bytes,10);
entries=Usage.directoryEntries(result,result.nodes.get('/'),true);
assert.equal(entries.find(e=>e.name==='denied').bytes,null);
assert.equal(entries.find(e=>e.name==='excluded').bytes,null);
assert.equal(entries.find(e=>e.name==='unknown').bytes,null);
assert.equal(entries.find(e=>e.name==='empty').bytes,0);
for(const n of result.nodes.values()) {
  entries=Usage.directoryEntries(result,n,true);
  assert.equal(entries.reduce((sum,e)=>sum+(e.bytes||0),0),result.host.get(n.path).allocated,n.path);
}
// A container reference to a host file claims the physical target as well.
fixture.tree.children[0].children.push(node('/alias',0,[],{kind:'reference',reference:'/srv/host/file'}));
fixture.containers.push(container('b','bob','/alias'));
fixture.resources.push(resource('/alias',['b'],['writable']));
result=Usage.build(fixture);
assert.equal(result.unrelated,70);
assert.equal(result.host.get('/srv/host').allocated,40);
assert(!result.host.get('/srv/host/file').visible);
assert.equal(result.host.get('@root').allocated,result.unrelated);
assert.equal(Usage.hostSources(fixture,result)[0].bytes,70);
assert.equal(Usage.hostSources(fixture,result)[1].bytes,200);
assert.equal(Usage.directoryEntries(result,fixture.tree,true)[0].name,'/');
// Multiple compact scan roots remain distinct, with no duplicated path prefix.
fixture=data([node('/one/cache',7),node('/two/cache',9)],[]);
result=Usage.build(fixture);
assert.deepEqual(Usage.directoryEntries(result,fixture.tree,true).map(e=>e.name),['/two/cache','/one/cache']);
assert.equal(result.host.get('@root').allocated,16);
console.log('Host accounting checks passed: nested container exclusions, hard-link claims, per-directory conservation, folded and unknown entries, host-only scans and distinct root paths.');

// One-level lazy reads retain a historical pool while exposing real, unmeasured
// child directories. The pool has an actionable identity without inventing
// proportional child sizes or losing their already observed metadata bytes.
fixture=data([node('/lazy',100,[
  node('/lazy/first',4,[],{size_unknown:true}),
  node('/lazy/second',4,[],{size_unknown:true}),
  node('/lazy/known',20,[],{omitted_entries:1})
],{scanning:true})],[container('a','alice','/lazy')]);
result=Usage.build(fixture);
entries=Usage.directoryEntries(result,result.nodes.get('/lazy'));
const historical=entries.find(e=>e.kind==='residual');
assert(historical.pending);
assert(historical.partial);
assert.equal(historical.bytes,80);
assert.equal(entries.find(e=>e.name==='first').bytes,null);
assert.equal(entries.find(e=>e.name==='first').node.path,'/lazy/first');
assert.equal(entries.reduce((sum,e)=>sum+(e.bytes||0),0),100);
assert.equal(Usage.mapEntries(entries).length,2,'unknown children have no fabricated map area');
entries=Usage.directoryEntries(result,result.nodes.get('/lazy/first'));
assert.equal(entries[0].bytes,null);
assert.equal(entries[0].known,false);
assert.equal(result.inspect('/lazy/first').known,true,'an unknown directory remains readable and navigable');
conserved(fixture,result);
console.log('Lazy usage checks passed: historical pools retain navigation metadata, unknown child sizes remain unknown, and map areas conserve bytes.');
