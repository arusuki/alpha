'use strict';
const assert = require('assert');
const SnapshotData = require('../dist/snapshot.js');
const Usage = require('../dist/usage.js');
const node = (path, allocated, children = [], extra = {}) => ({path,name:path.split('/').pop(),kind:'directory',allocated,apparent:allocated,files:1,errors:0,children,...extra});
const unrelated = node('/unrelated',4096);
const folded = node('/data/cache',8192,[],{omitted_entries:20});
const base = SnapshotData.validate({schema_version:2,scan:{omitted_references:0},job_id:'a'.repeat(32),revision:0,containers:[],resources:[],filesystems:[],warnings:[],tree:node('@root',16384,[node('/data',12288,[folded]),unrelated],{kind:'root'})});
const metadata = {...base,revision:1,updated_at:'2026-09-13T12:00:00Z'};
delete metadata.tree;
const scalars = n => { const {children,...row}=n; return row; };
const patch = {
  job_id:base.job_id,base_revision:0,revision:1,metadata,
  replacements:[node('/data/cache',8192,[node('/data/cache/deeper',4096,[],{size_unknown:true})],{scanning:true})],
  ancestors:[scalars(base.tree),scalars(base.tree.children[0])]
};
const before = JSON.stringify(base);
const updated = SnapshotData.applyChanges(base,patch);
assert.equal(JSON.stringify(base),before,'merging does not mutate the previous revision');
assert.equal(updated.tree.children[1],unrelated,'unrelated subtree is retained by reference');
assert.notEqual(updated.tree,base.tree);
assert.equal(updated.tree.children[0].children[0].omitted_entries,undefined,'replacement clears obsolete folded flags');
assert.equal(updated.revision,1);
const model = Usage.build(updated);
assert(model.nodes.has('/data/cache/deeper'));
assert.equal(model.unrelated,16384,'attribution still accounts for all historical bytes');

// Follow-up replacement consumes the historical budget and fixes every parent.
const deeper = node('/data/cache/deeper',4096,[node('/data/cache/deeper/file.bin',4096,[],{kind:'file'})]);
const next = {
  ...patch,base_revision:1,revision:2,metadata:{...metadata,revision:2},replacements:[deeper],
  ancestors:[scalars(node('@root',12288,[],{kind:'root'})),scalars(node('/data',8192)),scalars(node('/data/cache',4096,[],{}))]
};
const complete = SnapshotData.applyChanges(updated,next);
assert.equal(complete.tree.allocated,12288);
assert.equal(Usage.build(complete).unrelated,12288);
assert.equal(Usage.build(complete).nodes.get('/data/cache').scanning,undefined);
assert.equal(Usage.build(complete).nodes.get(deeper.path).size_unknown,undefined);
assert.equal(Usage.build(complete).nodes.get(deeper.path).children[0].kind,'file');

const unchanged = SnapshotData.applyChanges(complete,{...next,base_revision:2,replacements:[],ancestors:[]});
assert.equal(unchanged.tree,complete.tree);
for (const bad of [
  {...patch,job_id:'other'},
  {...patch,base_revision:1},
  {...patch,revision:-1},
  {...patch,metadata:{...metadata,tree:base.tree}},
  {...patch,ancestors:[]},
  {...patch,ancestors:[...patch.ancestors,patch.ancestors[0]]},
  {...patch,replacements:[node('/data/missing',1)]},
  {...patch,replacements:[node('/data/cache',1,[node('/data/cache/too-large',100)])]},
]) {
  assert.throws(()=>SnapshotData.applyChanges(base,bad));
  assert.equal(JSON.stringify(base),before,'invalid response leaves displayed data untouched');
}
assert.throws(()=>SnapshotData.applyChanges(updated,patch),'a late previous revision cannot overwrite newer details');
console.log('Snapshot patch checks passed: repeated drilldown, unchanged branch reuse, accounting, stale and malformed responses, atomic updates.');
