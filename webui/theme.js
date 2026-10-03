// Apply before first paint. Storage may be unavailable in private or sandboxed browsers.
const themeMedia = window.matchMedia('(prefers-color-scheme: dark)');
let themePreference = 'system';
try { themePreference = localStorage.getItem('tproxy_theme') || 'system'; } catch (_) {}
if (!['light', 'dark', 'system'].includes(themePreference)) themePreference = 'system';

function applyTheme() {
    document.documentElement.dataset.theme = themePreference === 'system'
        ? (themeMedia.matches ? 'dark' : 'light') : themePreference;
    document.querySelectorAll('.theme-select').forEach(select => { select.value = themePreference; });
}
function setTheme(preference) {
    themePreference = ['light', 'dark', 'system'].includes(preference) ? preference : 'system';
    try { localStorage.setItem('tproxy_theme', themePreference); } catch (_) {}
    applyTheme();
}
applyTheme();
themeMedia.addEventListener('change', () => { if (themePreference === 'system') applyTheme(); });
window.addEventListener('storage', event => {
    if (event.key === 'tproxy_theme') {
        themePreference = ['light', 'dark', 'system'].includes(event.newValue) ? event.newValue : 'system';
        applyTheme();
    }
});
document.addEventListener('DOMContentLoaded', applyTheme);

// A completion-based timer permits only one refresh per endpoint. Hidden pages
// and login screens stop polling, and resume with fresh data when visible.
const dashboardPollers = [];
let dashboardAuthPaused = false;
function startDashboardPoll(task, interval) {
    const poller = { timer: null, busy: false, run: null };
    poller.run = async () => {
        clearTimeout(poller.timer);
        if (document.hidden || dashboardAuthPaused || poller.busy) return;
        poller.busy = true;
        try { await task(); } catch (error) { console.error('Dashboard refresh failed:', error); }
        finally {
            poller.busy = false;
            if (!document.hidden && !dashboardAuthPaused) poller.timer = setTimeout(poller.run, interval);
        }
    };
    dashboardPollers.push(poller);
    poller.run();
}
function pauseDashboardPolling() {
    dashboardAuthPaused = true;
    dashboardPollers.forEach(poller => clearTimeout(poller.timer));
}
function resumeDashboardPolling() {
    dashboardAuthPaused = false;
    dashboardPollers.forEach(poller => poller.run());
}
document.addEventListener('visibilitychange', () => {
    dashboardPollers.forEach(poller => {
        clearTimeout(poller.timer);
        if (!document.hidden && !dashboardAuthPaused) poller.run();
    });
});

// Skip unchanged table, log and chart DOM replacements.
const renderedHTML = new WeakMap();
function setHTMLIfChanged(element, html) {
    if (renderedHTML.get(element) === html) return false;
    element.innerHTML = html;
    renderedHTML.set(element, html);
    return true;
}
