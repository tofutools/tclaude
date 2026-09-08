(()=>{
'use strict';
// Raw JSON is created only from explicit JSON inputs, never inferred from keys
// in user data. Keep it out of persistent browser draft objects; those carry
// DefaultJSON text and are prepared immediately before serialization.
class ExactJSON {
  constructor(text) { JSON.parse(text); this.text=text; Object.freeze(this); }
}

// Encode the JSON-shaped API payload while retaining explicit raw fragments.
// A replacer cannot insert raw JSON on browsers without JSON.rawJSON support.
function stringifyExact(value, space=0) {
  const stack=new Set(), indent=' '.repeat(Math.min(10,Math.max(0,space)));
  const encode=(value,depth,array=false)=>{
    if(value instanceof ExactJSON)return value.text;
    if(value&&typeof value.toJSON==='function')value=value.toJSON();
    if(value===null||typeof value!=='object')return JSON.stringify(value)??(array?'null':undefined);
    if(stack.has(value))throw new TypeError('Cyclic JSON value');
    stack.add(value);
    const parts=[];
    if(Array.isArray(value)){for(let i=0;i<value.length;i++)parts.push(encode(value[i],depth+1,true))}
    else for(const key of Object.keys(value)){const encoded=encode(value[key],depth+1);if(encoded!==undefined)parts.push(JSON.stringify(key)+':'+(indent?' ':'')+encoded)}
    stack.delete(value);
    const [open,close]=Array.isArray(value)?['[',']']:['{','}'];
    return open+(parts.length&&indent?'\n'+indent.repeat(depth+1):'')+parts.join(indent?',\n'+indent.repeat(depth+1):',')+(parts.length&&indent?'\n'+indent.repeat(depth):'')+close;
  };
  return encode(value,0);
}

function parameterDefaultText(parameter) {
  if(typeof parameter?.DefaultJSON==='string')return parameter.DefaultJSON;
  return parameter?.Default===undefined||parameter.Default===null?'':JSON.stringify(parameter.Default);
}

function wireDefinitionDraft(draft,{exporting=false}={}) {
  return {...draft,Parameters:(draft.Parameters||[]).map(parameter=>{
    const p={...parameter};
    if(typeof p.DefaultJSON==='string'){
      if(p.DefaultJSON.trim())p.Default=new ExactJSON(p.DefaultJSON);else delete p.Default;
    }
    if(!exporting)delete p.DefaultJSON;
    return p;
  })};
}

globalThis.ExactJSONTools={ExactJSON,stringifyExact,parameterDefaultText,wireDefinitionDraft};
})();
