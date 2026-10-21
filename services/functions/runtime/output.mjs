import { formatWithOptions } from 'node:util';
let installed=false;
// The root supervisor supplies immutable identity and app scope independently.
export function installOutputConsole(){
 if(installed)return;installed=true;
 for(const [name,level,stream] of [['debug','DEBUG',process.stdout],['log','INFO',process.stdout],['info','INFO',process.stdout],['warn','WARN',process.stderr],['error','ERROR',process.stderr]]){
  console[name]=(...args)=>{
   const message=formatWithOptions({colors:false,depth:4,maxArrayLength:32,maxStringLength:4096},...args);
   stream.write('@neon-app-log-v1:'+JSON.stringify({level,message})+'\n');
  };
 }
}
