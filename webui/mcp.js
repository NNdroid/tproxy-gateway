let mcpAuthSettings = null;
let mcpAuthBusy = false;
let mcpAuthGeneration = 0;
const mcpCredentialIDs = ['mcp-auth-token', 'mcp-auth-api-key-value', 'mcp-auth-password'];

function updateMCPAuthFields() {
    const mode = document.getElementById('mcp-auth-mode').value;
    document.getElementById('mcp-auth-bearer').hidden = mode !== 'bearer';
    document.getElementById('mcp-auth-api-key').hidden = mode !== 'api_key';
    document.getElementById('mcp-auth-basic').hidden = mode !== 'basic';
    const guidance = {
        inherit: ['复用 ui.secret，支持 Bearer 和 X-UI-Token 请求头。', 'Authorization: Bearer <ui.secret>'],
        bearer: ['使用独立 MCP 令牌；更换后新请求需要使用新令牌。', 'Authorization: Bearer <MCP_TOKEN>'],
        api_key: ['客户端在指定请求头中发送密钥。', (document.getElementById('mcp-auth-key-header').value || 'X-API-Key') + ': <MCP_API_KEY>'],
        basic: ['客户端使用 HTTP Basic 用户名和密码。跨网络传输请使用 HTTPS。', 'Authorization: Basic <base64(username:password)>'],
        none: ['客户端无需发送凭据；网关修改仍由 mcp.allow_writes 控制。', '无需认证请求头']
    };
    const [hint, example] = guidance[mode] || guidance.inherit;
    document.getElementById('mcp-auth-hint').textContent = hint;
    document.getElementById('mcp-auth-example').textContent = example;
}

function clearMCPAuthCredentials() {
    mcpAuthGeneration++;
    mcpCredentialIDs.forEach(id => { const input = document.getElementById(id); input.value = ''; input.type = 'password'; });
    document.querySelectorAll('.mcp-credential-row button').forEach(button => { if (button.textContent === '隐藏') button.textContent = '显示'; });
    mcpAuthSettings = null;
    document.getElementById('mcp-auth-save').disabled = true;
}

function showMCPAuthBanner(message, error = false) {
    const banner = document.getElementById('mcp-auth-banner');
    banner.className = 'editor-banner ' + (error ? 'banner-error' : 'banner-success');
    banner.textContent = message;
    banner.style.display = 'block';
}

function renderMCPAuthSettings(settings, clearCredentials = true) {
    if (clearCredentials) clearMCPAuthCredentials();
    mcpAuthSettings = settings;
    document.getElementById('mcp-auth-mode').value = settings.mode;
    document.getElementById('mcp-auth-key-header').value = settings.api_key_header || 'X-API-Key';
    document.getElementById('mcp-auth-username').value = settings.username || '';
    for (const [id, configured] of [['mcp-auth-token', settings.token_configured], ['mcp-auth-api-key-value', settings.api_key_configured], ['mcp-auth-password', settings.password_configured]]) {
        document.getElementById(id).placeholder = configured ? '已保存；留空保留原值' : '尚未设置，请输入凭据';
    }
    document.getElementById('mcp-auth-save').disabled = mcpAuthBusy;
    updateMCPAuthFields();
}

async function loadMCPAuthSettings() {
    if (mcpAuthBusy) return;
    const generation = ++mcpAuthGeneration;
    try {
        const response = await authFetch('/api/mcp/settings');
        const settings = await response.json();
        if (generation !== mcpAuthGeneration) return;
        renderMCPAuthSettings(settings);
        document.getElementById('mcp-auth-banner').style.display = 'none';
    } catch (error) {
        if (error.message !== 'Unauthorized') showMCPAuthBanner('无法读取 MCP 认证设置，请重新加载。', true);
    }
}

function buildMCPAuthUpdate() {
    const mode = document.getElementById('mcp-auth-mode').value;
    const update = { mode };
    if (mode === 'bearer') {
        const token = document.getElementById('mcp-auth-token').value;
        if (!token && !mcpAuthSettings?.token_configured) throw Error('请输入 Bearer Token 或生成令牌。');
        if (token) update.token = token;
    }
    if (mode === 'api_key') {
        update.api_key_header = document.getElementById('mcp-auth-key-header').value.trim() || 'X-API-Key';
        const key = document.getElementById('mcp-auth-api-key-value').value;
        if (!key && !mcpAuthSettings?.api_key_configured) throw Error('请输入 API Key 或生成密钥。');
        if (key) update.api_key = key;
    }
    if (mode === 'basic') {
        update.username = document.getElementById('mcp-auth-username').value;
        const password = document.getElementById('mcp-auth-password').value;
        if (!update.username || !password && !mcpAuthSettings?.password_configured) throw Error('请输入用户名和密码。');
        if (password) update.password = password;
    }
    return update;
}

async function saveMCPAuthSettings() {
    if (mcpAuthBusy || !mcpAuthSettings) return;
    let update;
    try { update = buildMCPAuthUpdate(); } catch (error) { showMCPAuthBanner(error.message, true); return; }
    mcpAuthBusy = true;
    const generation = mcpAuthGeneration;
    document.getElementById('mcp-auth-save').disabled = true;
    try {
        const response = await authFetch('/api/mcp/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(update) });
        const result = await response.json();
        if (generation !== mcpAuthGeneration) return;
        if (!response.ok) throw Error(result.message || '保存失败，原认证设置保持不变。');
        renderMCPAuthSettings(result, false);
        showMCPAuthBanner(result.preview ? '预览模式：仅模拟设置，不会修改网关认证。' : '已保存并立即生效。请在 MCP 客户端中使用对应认证方式；新密钥请妥善保存。');
    } catch (error) {
        if (error.message !== 'Unauthorized') showMCPAuthBanner(error.message, true);
    } finally {
        mcpAuthBusy = false;
        document.getElementById('mcp-auth-save').disabled = !mcpAuthSettings;
    }
}

function generateMCPCredential(id) {
    const random = new Uint8Array(32);
    crypto.getRandomValues(random);
    document.getElementById(id).value = Array.from(random, byte => byte.toString(16).padStart(2, '0')).join('');
}

function toggleMCPCredential(id, button) {
    const input = document.getElementById(id);
    input.type = input.type === 'password' ? 'text' : 'password';
    button.textContent = input.type === 'password' ? '显示' : '隐藏';
}

function initializeMCPAuthSettings() {
    document.getElementById('mcp-auth-key-header').addEventListener('input', updateMCPAuthFields);
    loadMCPAuthSettings();
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initializeMCPAuthSettings);
else initializeMCPAuthSettings();
