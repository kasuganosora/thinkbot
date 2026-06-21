import{D as r,d as m,o as a,c,n as p,w as b,M as k,u as o,a as d,J as h,m as f,b as n,N as x,K as _,_ as V}from"./index-Bl0_US1g.js";/**
 * @license lucide-vue-next v0.577.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const C=r("check",[["path",{d:"M20 6 9 17l-5-5",key:"1gmf2c"}]]),v=["disabled"],y={class:"tcheckbox-box"},B={key:0,class:"tcheckbox-label"},M=m({__name:"TCheckbox",props:{modelValue:{type:Boolean},disabled:{type:Boolean}},emits:["update:modelValue"],setup(s,{emit:i}){const e=_(s,"modelValue",i,{passive:!0,defaultValue:!1});return(t,l)=>(a(),c("label",{class:p(["tcheckbox",{disabled:s.disabled}])},[b(d("input",{type:"checkbox","onUpdate:modelValue":l[0]||(l[0]=u=>h(e)?e.value=u:null),disabled:s.disabled,class:"tcheckbox-input"},null,8,v),[[k,o(e)]]),d("span",y,[o(e)?(a(),f(o(C),{key:0,size:12,"stroke-width":"3"})):n("",!0)]),t.$slots.default?(a(),c("span",B,[x(t.$slots,"default",{},void 0)])):n("",!0)],2))}}),z=V(M,[["__scopeId","data-v-30478402"]]);export{z as T};
