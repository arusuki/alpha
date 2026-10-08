// Browser fixtures only: independently project existing scan fixtures to the
// server view contract. Production accounting is implemented in Go.
const Usage=require('../dist/usage.js');
function viewFixture(data,path='') {
 const u=Usage.build(data), nodes=new Map();
 function add(n) {
  if(!n || nodes.has(n.path))return;
  const {children,...row}=n;
  nodes.set(n.path,{...row,children:[],partial:u.incomplete.has(n.path),host:u.host.get(n.path)});
  if(n.kind==='reference')add(u.nodes.get(n.reference));
 }
 function open(n) {if(!n)return;add(n);n.children.forEach(add);Object.assign(nodes.get(n.path),{children:n.children.map(n=>n.path),loaded:true});}
 open(data.tree);
 for(const c of data.containers){add(u.nodes.get(c.upper_path));if(c.log_path)add(u.nodes.get(c.log_path.slice(0,c.log_path.lastIndexOf('/'))||'/'));for(const m of c.mounts)add(u.nodes.get(m.source));}
 for(const r of data.resources)add(u.nodes.get(r.path));
 add(u.nodes.get(path));open(u.inspect(path || data.tree.path).node);
 for(let parent=path.slice(0,path.lastIndexOf('/'))||'/';parent.startsWith('/');parent=parent.slice(0,parent.lastIndexOf('/'))||'/'){add(u.nodes.get(parent));if(parent==='/')break;}
 const {tree,...metadata}=data;
 return {view_version:1,metadata,root:tree.path,nodes:[...nodes.values()],usage:{exclusive:u.exclusive,shared:u.shared,crossOwner:u.crossOwner,unrelated:u.unrelated,attributionLimited:u.attributionLimited,containers:Object.fromEntries(u.containers),owners:Object.fromEntries(u.owners)}};
}
module.exports=viewFixture;
if(require.main===module){let input='';process.stdin.on('data',chunk=>input+=chunk);process.stdin.on('end',()=>{const {data,path}=JSON.parse(input);process.stdout.write(JSON.stringify(viewFixture(data,path)));});}
