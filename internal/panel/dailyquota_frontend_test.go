package panel

import "testing"

// TestDailyQuotaFrontend 执行真实 app.js，覆盖账号设置的回显、校验、部分保存和继承清除。
func TestDailyQuotaFrontend(t *testing.T) {
	proxyUIRunNode(t, `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const inert = new Proxy(function(){}, {get(t,k){ if(k===Symbol.toPrimitive)return ()=>''; return inert; },set(){return true;},apply(){return inert;}});
const elements = new Map();
function el(id) {
  if (!elements.has(id)) elements.set(id, new Proxy({value:'',disabled:false,hidden:false,textContent:'',innerHTML:'',
    classList:{add(){},remove(){},contains(){return false;}},focus(){},addEventListener(){}}, {get(t,k){return k in t?t[k]:inert;},set(t,k,v){t[k]=k==='value'?String(v):v;return true;}}));
  return elements.get(id);
}
const ctx = {
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError,
  Symbol, Proxy, Reflect, URL, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
  location:{hash:'#accounts'}, history:{replaceState(){}}, localStorage:{getItem(){return null;},setItem(){}},
  navigator:{clipboard:{writeText:async()=>{}}},
  document:{getElementById:el,querySelectorAll:()=>[],querySelector:()=>inert,addEventListener(){},documentElement:inert,head:inert,body:inert,createElement:()=>inert,cookie:''},
  fetch:()=>new Promise(()=>{}), addEventListener(){}, removeEventListener(){},
  matchMedia:()=>({matches:false,addEventListener(){}}),setInterval:()=>1,clearInterval(){},setTimeout:()=>1,clearTimeout(){}
};
ctx.window=ctx;ctx.globalThis=ctx;vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2],'utf8'),ctx);
const calls=[];
ctx.api=async(path,options)=>{calls.push({path,body:JSON.parse(options.body)});return {ok:true};};
ctx.loadOverview=async()=>{};ctx.toast=()=>{};
vm.runInContext('accLast=[{uid:"u1",nickname:"测试账号",daily_quota:{used:1.5,limit:2,override:2,exceeded:false}}]',ctx);
(async()=>{
  ctx.openDailyQuota('u1');
  assert.equal(el('dailyQuotaMode').value,'custom');
  assert.equal(el('dailyQuotaLimit').value,2);
  el('dailyQuotaLimit').value='-1';
  await ctx.saveDailyQuota();
  assert.equal(calls.length,0);
  assert.equal(el('dailyQuotaError').hidden,false);
  el('dailyQuotaLimit').value='0.25';
  await ctx.saveDailyQuota();
  assert.deepEqual(calls.pop(),{path:'config',body:{pool:{account_daily_credit_limits:{u1:0.25}}}});
  ctx.openDailyQuota('u1');el('dailyQuotaMode').value='inherit';
  await ctx.saveDailyQuota();
  assert.equal(calls.pop().body.pool.account_daily_credit_limits.u1,null);
  ctx.openDailyQuota('u1');el('dailyQuotaMode').value='unlimited';
  await ctx.saveDailyQuota();
  assert.equal(calls.pop().body.pool.account_daily_credit_limits.u1,0);
  ctx.renderAccounts([{uid:'u1',daily_quota:{used:2,limit:2,override:null,exceeded:true,usage_incomplete:true}}]);
  assert(el('accBody').innerHTML.includes('已达每日限额'));
  assert(el('accBody').innerHTML.includes('用量不完整'));
  ctx.api=async()=>{throw new Error('保存失败');};ctx.openDailyQuota('u1');
  await ctx.saveDailyQuota();
  assert.equal(el('dailyQuotaError').hidden,false);
  assert.equal(el('btnDailyQuotaSave').disabled,false);
  process.exit(0);
})().catch(e=>{console.error(e);process.exit(1);});`)
}
