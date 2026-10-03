const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../webui/theme.js'), 'utf8');

function harness({ stored = null, dark = false, storageFails = false } = {}) {
    const events = {}, timers = new Map(), selectors = [{ value: null }, { value: null }];
    let id = 0;
    const media = { matches: dark, addEventListener: (name, callback) => { events.media = callback; } };
    const document = {
        hidden: false, documentElement: { dataset: {} },
        querySelectorAll: () => selectors,
        addEventListener: (name, callback) => { events[name] = callback; }
    };
    const context = vm.createContext({ document, console, WeakMap,
        window: { matchMedia: () => media, addEventListener: (name, callback) => { events[name] = callback; } },
        localStorage: {
            getItem: () => { if (storageFails) throw Error('storage blocked'); return stored; },
            setItem: (_, value) => { if (storageFails) throw Error('storage blocked'); stored = value; }
        },
        setTimeout: (callback) => { timers.set(++id, callback); return id; },
        clearTimeout: id => timers.delete(id)
    });
    vm.runInContext(source, context);
    return { context, document, media, events, timers, selectors };
}
const settle = () => new Promise(setImmediate);

test('theme follows system, explicit choices persist, and selectors stay in sync', () => {
    const h = harness({ dark: true });
    assert.equal(h.document.documentElement.dataset.theme, 'dark');
    h.context.setTheme('light');
    h.media.matches = true;
    h.events.media();
    assert.equal(h.document.documentElement.dataset.theme, 'light');
    assert.deepEqual(h.selectors.map(s => s.value), ['light', 'light']);
    h.events.storage({ key: 'tproxy_theme', newValue: 'dark' });
    assert.equal(h.document.documentElement.dataset.theme, 'dark');
    h.events.storage({ key: 'tproxy_theme', newValue: null });
    h.media.matches = false;
    h.events.media();
    assert.equal(h.document.documentElement.dataset.theme, 'light');
});

test('unavailable storage and invalid preferences have a working fallback', () => {
    for (const options of [{ storageFails: true }, { stored: 'invalid' }]) {
        const h = harness(options);
        h.context.setTheme('dark');
        assert.equal(h.document.documentElement.dataset.theme, 'dark');
        h.context.setTheme('system');
        assert.equal(h.document.documentElement.dataset.theme, 'light');
    }
});

test('polling waits for completion and pauses while hidden or unauthenticated', async () => {
    const h = harness();
    let calls = 0, release;
    h.context.startDashboardPoll(() => { calls++; return new Promise(resolve => { release = resolve; }); }, 2000);
    h.context.resumeDashboardPolling();
    h.events.visibilitychange();
    assert.equal(calls, 1);
    assert.equal(h.timers.size, 0);
    release();
    await settle();
    assert.equal(h.timers.size, 1);
    h.document.hidden = true;
    h.events.visibilitychange();
    assert.equal(h.timers.size, 0);
    h.document.hidden = false;
    h.events.visibilitychange();
    assert.equal(calls, 2);
    h.context.pauseDashboardPolling();
    release();
    await settle();
    assert.equal(h.timers.size, 0);
    h.context.resumeDashboardPolling();
    assert.equal(calls, 3);
    release();
    await settle();
});

test('unchanged HTML avoids another DOM replacement', () => {
    const h = harness();
    let writes = 0;
    const element = { set innerHTML(_) { writes++; } };
    h.context.setHTMLIfChanged(element, '<p>one</p>');
    h.context.setHTMLIfChanged(element, '<p>one</p>');
    h.context.setHTMLIfChanged(element, '<p>empty</p>');
    h.context.setHTMLIfChanged(element, '<p>one</p>');
    assert.equal(writes, 3);
});

const html = fs.readFileSync(path.join(__dirname, '../webui/index.html'), 'utf8');
const authSource = html.slice(html.indexOf('        async function authFetch'), html.indexOf('        function showLoginModal'));

test('overlapping GETs share the request and retain separate response bodies', async () => {
    let calls = 0, resolve;
    const context = vm.createContext({ Headers, AbortController, setTimeout, clearTimeout,
        getStoredToken: () => 'test-only',
        fetch: () => { calls++; return new Promise(r => { resolve = r; }); },
        showLoginModal: () => {}
    });
    vm.runInContext('const pendingGets = new Map(); let authGeneration = 0;\n' + authSource, context);
    const a = context.authFetch('/api/state'), b = context.authFetch('/api/state');
    assert.equal(calls, 1);
    resolve(new Response('{"status":"running"}'));
    const responses = await Promise.all([a, b]);
    assert.deepEqual(await responses[0].json(), await responses[1].json());
});

test('HTTP failures and stale authentication responses do not update the dashboard', async () => {
    let status = 500, modal = 0;
    const context = vm.createContext({ Headers, AbortController, setTimeout, clearTimeout,
        getStoredToken: () => '', fetch: async () => new Response('{}', { status }),
        showLoginModal: () => { modal++; }
    });
    vm.runInContext('const pendingGets = new Map(); let authGeneration = 0;\n' + authSource, context);
    await assert.rejects(context.authFetch('/api/state'), /HTTP 500/);
    status = 401;
    await assert.rejects(context.authFetch('/api/state'), /Unauthorized/);
    assert.equal(modal, 1);
    status = 200;
    const pending = context.authFetch('/api/state');
    vm.runInContext('authGeneration++', context);
    await assert.rejects(pending, /Unauthorized/);
    status = 400;
    const rejectedConfig = await context.authFetch('/api/config', { method: 'POST' });
    assert.equal(rejectedConfig.status, 400);
    assert.deepEqual(await rejectedConfig.json(), {});
});
