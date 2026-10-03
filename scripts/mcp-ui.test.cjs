const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const source = fs.readFileSync(path.join(__dirname, '../webui/mcp.js'), 'utf8');

function harness(fetch) {
    const fields = new Map();
    const document = { readyState: 'loading', addEventListener: () => {}, querySelectorAll: () => [],
        getElementById: id => { if (!fields.has(id)) fields.set(id, { value:'', type:'password', disabled:false, style:{}, textContent:'', addEventListener:()=>{} }); return fields.get(id); } };
    const context = vm.createContext({ document, console, Uint8Array, crypto:webcrypto, authFetch:fetch,
        localStorage:{setItem:()=>{throw Error('MCP secrets must not be stored in browser storage');}} });
    vm.runInContext(source,context);
    context.renderMCPAuthSettings({mode:'inherit', api_key_header:'X-API-Key', username:'stored-user', token_configured:true, password_configured:true});
    return {context, fields, element:id=>document.getElementById(id)};
}

test('auth mode chooses its fields and blank saved secrets are omitted from updates', () => {
    const h = harness();
    h.element('mcp-auth-mode').value = 'basic';
    h.context.updateMCPAuthFields();
    assert.equal(h.element('mcp-auth-basic').hidden,false);
    assert.equal(h.element('mcp-auth-bearer').hidden,true);
    assert.deepEqual(JSON.parse(JSON.stringify(h.context.buildMCPAuthUpdate())), {mode:'basic', username:'stored-user'});
    h.element('mcp-auth-mode').value = 'api_key';
    assert.throws(()=>h.context.buildMCPAuthUpdate(),/API Key/);
    h.element('mcp-auth-api-key-value').value = 'test-key';
    h.element('mcp-auth-key-header').value = 'X-Gateway-Key';
    h.context.updateMCPAuthFields();
    assert.equal(h.element('mcp-auth-example').textContent,'X-Gateway-Key: <MCP_API_KEY>');
    assert.deepEqual(JSON.parse(JSON.stringify(h.context.buildMCPAuthUpdate())),{mode:'api_key',api_key_header:'X-Gateway-Key',api_key:'test-key'});
});

test('save emits administrator PUT, preserves entered credentials, and reload clears secrets', async () => {
    const calls=[];
    const h = harness(async (url,options) => { calls.push({url,options}); return new Response(JSON.stringify({mode:'bearer',token_configured:true})); });
    h.element('mcp-auth-mode').value='bearer';
    h.element('mcp-auth-token').value='new-test-token';
    await h.context.saveMCPAuthSettings();
    assert.equal(calls[0].url,'/api/mcp/settings');
    assert.equal(calls[0].options.method,'PUT');
    assert.deepEqual(JSON.parse(calls[0].options.body),{mode:'bearer',token:'new-test-token'});
    assert.equal(h.element('mcp-auth-token').value,'new-test-token');
    assert.match(h.element('mcp-auth-banner').textContent,/立即生效/);
    await h.context.loadMCPAuthSettings();
    assert.equal(h.element('mcp-auth-token').value,'');
    assert.match(h.element('mcp-auth-token').placeholder,/已保存/);
});

test('failed saves remain editable and overlapping submissions make one request', async () => {
    let resolve,calls=0;
    const h = harness(()=>{calls++;return new Promise(r=>{resolve=r;});});
    h.element('mcp-auth-mode').value='none';
    const pending=h.context.saveMCPAuthSettings();
    await h.context.saveMCPAuthSettings();
    assert.equal(calls,1);
    resolve(new Response(JSON.stringify({message:'disk save failed'}),{status:500}));
    await pending;
    assert.equal(h.element('mcp-auth-save').disabled,false);
    assert.equal(h.element('mcp-auth-mode').value,'none');
    assert.match(h.element('mcp-auth-banner').textContent,/disk save failed/);
});

test('logout clears credentials and ignores a late successful save response', async () => {
    let resolve;
    const h=harness(()=>new Promise(r=>{resolve=r;}));
    h.element('mcp-auth-mode').value='bearer';
    h.element('mcp-auth-token').value='test-secret';
    const pending=h.context.saveMCPAuthSettings();
    h.context.clearMCPAuthCredentials();
    resolve(new Response(JSON.stringify({mode:'bearer',token_configured:true})));
    await pending;
    assert.equal(h.element('mcp-auth-save').disabled,true);
    assert.equal(h.element('mcp-auth-token').value,'');
});

test('generated credentials use 32 random bytes and remain outside browser storage', () => {
    const h=harness();
    h.context.generateMCPCredential('mcp-auth-token');
    const first=h.element('mcp-auth-token').value;
    assert.match(first,/^[0-9a-f]{64}$/);
    h.context.generateMCPCredential('mcp-auth-token');
    assert.notEqual(first,h.element('mcp-auth-token').value);
});
