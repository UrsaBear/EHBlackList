// ==UserScript==
// @name         ExHentai / EH 黑名单管理 API版
// @namespace    https://exhentai.org/
// @version      3.5.0-api-settings
// @description  服务端黑名单: 通过本地 Go API 查询/管理 ID + 关键词黑名单，支持可配置后端地址/Token、原因反显、原因下拉选择、三态过滤、LRR 共存、管理面板、导入导出。
// @author       You
// @match        https://exhentai.org/*
// @match        https://e-hentai.org/*
// @icon         https://e-hentai.org/favicon.ico
// @grant        GM_addStyle
// @grant        GM_registerMenuCommand
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @connect      127.0.0.1
// @connect      localhost
// @connect      *
// @license      MIT
// ==/UserScript==

(function() {
    'use strict';

    // ========== 服务端配置 ==========

    // 默认留空：首次使用时通过页面提示填写，不再把内网地址写死在脚本里。
    const DEFAULT_API_BASE_URL = '';
    const SETTING_API_BASE_URL = 'ehbl-api-base-url';
    const SETTING_API_TOKEN = 'ehbl-api-token';

    let apiBaseUrl = '';
    let apiToken = '';

    try {
        apiBaseUrl = normalizeApiBaseUrl(readSetting(SETTING_API_BASE_URL, DEFAULT_API_BASE_URL));
        apiToken = String(readSetting(SETTING_API_TOKEN, '') || '').trim();
    } catch (e) {
        console.warn('[EH-BL API] 读取后端设置失败', e);
        apiBaseUrl = '';
    }

    function readSetting(key, fallback) {
        try {
            if (typeof GM_getValue === 'function') return GM_getValue(key, fallback);
        } catch (e) {
            console.warn('[EH-BL API] 读取设置失败', e);
        }
        return fallback;
    }

    function writeSetting(key, value) {
        try {
            if (typeof GM_setValue === 'function') {
                GM_setValue(key, value);
                return true;
            }
        } catch (e) {
            console.warn('[EH-BL API] 保存设置失败', e);
        }
        return false;
    }

    function normalizeApiBaseUrl(raw) {
        let value = String(raw == null ? '' : raw).trim();
        if (!value) return '';
        if (value.startsWith('//')) value = 'http:' + value;
        else if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(value)) value = 'http://' + value;

        let parsed;
        try {
            parsed = new URL(value);
        } catch (e) {
            throw new Error('后端地址格式不正确');
        }
        if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
            throw new Error('后端地址只支持 http/https');
        }
        if (parsed.username || parsed.password) {
            throw new Error('请勿在地址中填写用户名或密码');
        }
        if (parsed.search || parsed.hash) {
            throw new Error('后端地址不能包含查询参数或锚点');
        }
        return (parsed.origin + parsed.pathname).replace(/\/+$/, '');
    }

    function hasApiConfig() {
        return !!apiBaseUrl;
    }

    const FILTER_KEY = 'eh-blacklist-filter-mode'; // all / hide / only
    const LRR_FILTER_KEY = 'lrr-filter-mode'; // all / hide / only

    const PAGE_SIZE = 20;
    const API_LIST_LIMIT = 5000;

    let currentPage = 1;
    let currentSearchTerm = '';
    let activeTab = 'kw'; // 'kw' / 'id' / 'reason'
    let pageInitialized = false;

    let overlayEl;
    let panelTableBody;
    let importTextArea;
    let pageInfoSpan;
    let prevPageBtn;
    let nextPageBtn;
    let searchInput;
    let tabBtnKw;
    let tabBtnId;
    let tabBtnReason;

    let cachedReasons = null;
    let latestListResults = new Map(); // gid -> result
    let detailResult = null;

    // ========== API 工具 ==========

    function apiUrl(path) {
        if (/^https?:\/\//i.test(path)) return path;
        if (!apiBaseUrl) throw new Error('未配置后端地址');
        return apiBaseUrl + path;
    }

    function apiRequest(method, path, body) {
        return new Promise((resolve, reject) => {
            if (!apiBaseUrl) {
                const err = new Error('未配置后端地址，请先在“设置”中填写');
                err.code = 'NO_CONFIG';
                reject(err);
                return;
            }

            const headers = { 'Accept': 'application/json' };
            if (apiToken) headers['Authorization'] = 'Bearer ' + apiToken;
            const options = {
                method,
                url: apiUrl(path),
                headers,
                timeout: 30000,
                onload: response => {
                    const text = response.responseText || '';
                    let data = null;
                    if (text.trim()) {
                        try {
                            data = JSON.parse(text);
                        } catch (e) {
                            const err = new Error(`API 返回非 JSON: HTTP ${response.status} ${text.slice(0, 200)}`);
                            err.status = response.status;
                            reject(err);
                            return;
                        }
                    }
                    if (response.status < 200 || response.status >= 300) {
                        const err = new Error((data && data.error) || `HTTP ${response.status}`);
                        err.status = response.status;
                        err.data = data;
                        reject(err);
                        return;
                    }
                    resolve(data);
                },
                onerror: () => reject(new Error('API 网络错误')),
                ontimeout: () => reject(new Error('API 请求超时')),
                onabort: () => reject(new Error('API 请求被中止'))
            };
            if (body !== undefined && body !== null) {
                headers['Content-Type'] = 'application/json';
                options.data = JSON.stringify(body);
            }
            GM_xmlhttpRequest(options);
        });
    }

    function encodeQuery(params) {
        const sp = new URLSearchParams();
        Object.keys(params).forEach(k => {
            const v = params[k];
            if (v !== undefined && v !== null && v !== '') sp.set(k, String(v));
        });
        const s = sp.toString();
        return s ? '?' + s : '';
    }

    async function getReasons(force) {
        if (!force && cachedReasons) return cachedReasons;
        const data = await apiRequest('GET', '/reasons');
        cachedReasons = (data && data.items) || [];
        return cachedReasons;
    }

    async function createReasonIfNeeded(label) {
        const clean = String(label || '').trim();
        if (!clean) return null;
        const data = await apiRequest('POST', '/reasons', {
            label: clean,
            sort_order: 100
        });
        cachedReasons = null;
        return data;
    }

    async function upsertEntry(entry) {
        const data = await apiRequest('POST', '/entries', entry);
        return data;
    }

    async function deleteEntry(type, valueNorm) {
        return apiRequest('DELETE', '/entries' + encodeQuery({ type, value_norm: valueNorm }));
    }

    async function checkItems(items) {
        if (!items.length) return [];
        const data = await apiRequest('POST', '/check', { items });
        return (data && data.items) || [];
    }

    function showApiError(err, prefix) {
        console.error('[EH-BL API]', prefix || 'API 错误', err);
        let message = err && err.message ? err.message : String(err);
        if (err && err.status === 401) {
            message = '未授权：请打开“设置”填写正确的 API Token';
        } else if (err && err.code === 'NO_CONFIG') {
            message = '未配置后端地址：请打开“设置”填写后端地址';
        }
        alert((prefix || 'API 错误') + ': ' + message);
    }

    // ========== 后端设置 ==========

    let settingsMaskEl = null;
    let configBannerEl = null;

    function openSettings() {
        if (settingsMaskEl) return;

        const mask = document.createElement('div');
        mask.id = 'ehbl-settings-mask';
        Object.assign(mask.style, {
            position: 'fixed',
            inset: '0',
            zIndex: '100001',
            background: 'rgba(0,0,0,0.5)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center'
        });

        const box = document.createElement('div');
        Object.assign(box.style, {
            width: 'min(560px, 92vw)',
            maxHeight: '86vh',
            overflow: 'auto',
            background: '#f7f7f7',
            color: '#111',
            padding: '16px',
            borderRadius: '8px',
            boxShadow: '0 2px 12px rgba(0,0,0,0.45)',
            fontSize: '13px'
        });

        const title = document.createElement('div');
        title.textContent = '黑名单后端设置';
        title.style.fontWeight = 'bold';
        title.style.fontSize = '16px';
        title.style.marginBottom = '10px';
        box.appendChild(title);

        const hint = document.createElement('div');
        hint.textContent = '后端地址留空时不进行任何查询；例如 http://127.0.0.1:8787';
        hint.style.fontSize = '12px';
        hint.style.opacity = '0.75';
        hint.style.marginBottom = '10px';
        box.appendChild(hint);

        const addressLabel = document.createElement('div');
        addressLabel.textContent = '后端地址';
        addressLabel.style.fontWeight = 'bold';
        addressLabel.style.marginBottom = '4px';
        box.appendChild(addressLabel);

        const addressInput = document.createElement('input');
        addressInput.type = 'text';
        addressInput.className = 'ehbl-input';
        addressInput.placeholder = 'http://127.0.0.1:8787';
        addressInput.value = apiBaseUrl;
        addressInput.style.fontSize = '13px';
        addressInput.style.padding = '6px 8px';
        box.appendChild(addressInput);

        const tokenLabel = document.createElement('div');
        tokenLabel.textContent = 'API Token（可选）';
        tokenLabel.style.fontWeight = 'bold';
        tokenLabel.style.margin = '12px 0 4px 0';
        box.appendChild(tokenLabel);

        const tokenInput = document.createElement('input');
        tokenInput.type = 'password';
        tokenInput.className = 'ehbl-input';
        tokenInput.placeholder = '服务端设置 API_TOKEN 时填写';
        tokenInput.value = apiToken;
        tokenInput.autocomplete = 'off';
        tokenInput.style.fontSize = '13px';
        tokenInput.style.padding = '6px 8px';
        box.appendChild(tokenInput);

        const status = document.createElement('div');
        status.style.minHeight = '18px';
        status.style.marginTop = '8px';
        status.style.fontSize = '12px';
        box.appendChild(status);

        const actions = document.createElement('div');
        actions.style.display = 'flex';
        actions.style.justifyContent = 'flex-end';
        actions.style.gap = '8px';
        actions.style.marginTop = '12px';

        const testBtn = document.createElement('button');
        testBtn.type = 'button';
        testBtn.className = 'ehbl-inline-btn';
        testBtn.textContent = '测试连接';
        testBtn.addEventListener('click', () => {
            testConnection(addressInput.value.trim(), tokenInput.value.trim(), status);
        });

        const cancelBtn = document.createElement('button');
        cancelBtn.type = 'button';
        cancelBtn.className = 'ehbl-inline-btn';
        cancelBtn.textContent = '取消';
        cancelBtn.addEventListener('click', () => close());

        const saveBtn = document.createElement('button');
        saveBtn.type = 'button';
        saveBtn.className = 'ehbl-inline-btn';
        saveBtn.textContent = '保存';
        saveBtn.addEventListener('click', () => {
            saveSettings(addressInput.value, tokenInput.value, status, close);
        });

        actions.appendChild(testBtn);
        actions.appendChild(cancelBtn);
        actions.appendChild(saveBtn);
        box.appendChild(actions);

        const onKey = event => {
            if (event.key === 'Escape') {
                event.preventDefault();
                close();
            }
        };
        function close() {
            document.removeEventListener('keydown', onKey);
            mask.remove();
            settingsMaskEl = null;
        }

        mask.addEventListener('click', event => {
            if (event.target === mask) close();
        });
        document.addEventListener('keydown', onKey);

        mask.appendChild(box);
        document.body.appendChild(mask);
        settingsMaskEl = mask;

        addressInput.focus();
        addressInput.select();
    }

    function saveSettings(rawBase, rawToken, statusEl, close) {
        let nextBase = '';
        try {
            nextBase = normalizeApiBaseUrl(rawBase);
        } catch (e) {
            statusEl.textContent = e.message;
            statusEl.style.color = '#c00';
            return;
        }

        const nextToken = String(rawToken || '').trim();
        const wasConfigured = hasApiConfig();

        const storedBase = writeSetting(SETTING_API_BASE_URL, nextBase);
        const storedToken = writeSetting(SETTING_API_TOKEN, nextToken);
        const persisted = storedBase && storedToken;

        apiBaseUrl = nextBase;
        apiToken = nextToken;
        cachedReasons = null;

        close();

        if (!apiBaseUrl) {
            if (overlayEl) overlayEl.style.display = 'none';
            showConfigBanner();
            return;
        }

        hideConfigBanner();
        if (!wasConfigured && !pageInitialized) {
            init();
        } else {
            refreshCurrentPageMatches();
            if (overlayEl && overlayEl.style.display !== 'none') {
                renderTable();
            }
        }

        if (!persisted) {
            alert('设置已在当前页面生效，但无法持久化（GM 存储不可用）。');
        }
    }

    function testConnection(rawBase, rawToken, statusEl) {
        let base = '';
        try {
            base = normalizeApiBaseUrl(rawBase);
        } catch (e) {
            statusEl.textContent = e.message;
            statusEl.style.color = '#c00';
            return;
        }
        if (!base) {
            statusEl.textContent = '请先填写后端地址';
            statusEl.style.color = '#c00';
            return;
        }

        const token = String(rawToken || '').trim();
        statusEl.textContent = '测试中…';
        statusEl.style.color = '#555';

        GM_xmlhttpRequest({
            method: 'GET',
            url: base + '/stats',
            timeout: 10000,
            headers: token ? { 'Authorization': 'Bearer ' + token } : {},
            onload: response => {
                if (response.status >= 200 && response.status < 300) {
                    statusEl.textContent = '连接成功';
                    statusEl.style.color = '#080';
                } else if (response.status === 401) {
                    statusEl.textContent = '连接成功，但 Token 未通过认证';
                    statusEl.style.color = '#c00';
                } else {
                    statusEl.textContent = `连接失败：HTTP ${response.status}`;
                    statusEl.style.color = '#c00';
                }
            },
            onerror: () => {
                statusEl.textContent = '连接失败：网络错误';
                statusEl.style.color = '#c00';
            },
            ontimeout: () => {
                statusEl.textContent = '连接失败：请求超时';
                statusEl.style.color = '#c00';
            }
        });
    }

    function showConfigBanner() {
        if (configBannerEl || !document.body) return;
        const bar = document.createElement('div');
        bar.id = 'ehbl-config-banner';
        bar.textContent = '未配置黑名单后端，点击设置';
        Object.assign(bar.style, {
            position: 'fixed',
            left: '8px',
            bottom: '76px',
            zIndex: '9999',
            background: '#b3261e',
            color: '#fff',
            padding: '6px 10px',
            borderRadius: '4px',
            fontSize: '12px',
            cursor: 'pointer',
            boxShadow: '0 1px 4px rgba(0,0,0,0.3)'
        });
        bar.addEventListener('click', openSettings);
        document.body.appendChild(bar);
        configBannerEl = bar;
    }

    function hideConfigBanner() {
        if (configBannerEl) {
            configBannerEl.remove();
            configBannerEl = null;
        }
    }

    // ========== 通用工具 ==========

    function getFilterMode() {
        const v = localStorage.getItem(FILTER_KEY);
        if (v === 'all' || v === 'hide' || v === 'only') return v;
        return 'hide';
    }

    function setFilterMode(mode) {
        localStorage.setItem(FILTER_KEY, mode);
        applyListFilter();
    }

    function getLrrFilterMode() {
        const v = localStorage.getItem(LRR_FILTER_KEY);
        if (v === 'all' || v === 'hide' || v === 'only') return v;
        return 'all';
    }

    function recomputeRowDisplay(row) {
        const lrrHide = row.dataset.lrrHide === '1';
        const ehblHide = row.dataset.ehblHide === '1';
        row.style.display = (lrrHide || ehblHide) ? 'none' : '';
    }

    function createElement(tag, className, text) {
        const el = document.createElement(tag);
        if (className) el.className = className;
        if (text != null) el.textContent = text;
        return el;
    }

    function getGalleryIdFromUrl(url) {
        try {
            const m = url.match(/\/g\/(\d+)\/([0-9a-fA-F]+)\//);
            if (!m) return null;
            return m[1];
        } catch {
            return null;
        }
    }

    function getCurrentGalleryId() {
        return getGalleryIdFromUrl(location.href);
    }

    function stripOurBadges(text) {
        return String(text || '')
            .replace(/本作品在黑名单/g, '')
            .replace(/黑名单(?:-[A-Z]+)?(?:：[^\s]+)?/g, '')
            .trim();
    }

    function getMatchLabel(result) {
        if (!result || !result.blacklisted || !result.match) return '';
        const reason = result.match.reason_label || '';
        return reason ? `黑名单：${reason}` : '黑名单';
    }

    function getMatchTooltip(result) {
        if (!result || !result.blacklisted || !result.match) return '';
        const m = result.match;
        return [
            `类型: ${m.type === 'id' ? 'ID' : '关键词'}`,
            `值: ${m.value || ''}`,
            `标准化: ${m.value_norm || ''}`,
            `原因: ${m.reason_label || '无'}`,
            `优先级: ${m.priority}`
        ].join('\n');
    }

    function applyResultToRowDataset(row, result) {
        row.dataset.ehblBlacklisted = result && result.blacklisted ? '1' : '0';
        if (result && result.match) {
            row.dataset.ehblMatchType = result.match.type || '';
            row.dataset.ehblMatchValue = result.match.value || '';
            row.dataset.ehblMatchValueNorm = result.match.value_norm || '';
            row.dataset.ehblMatchReason = result.match.reason_label || '';
        } else {
            row.dataset.ehblMatchType = '';
            row.dataset.ehblMatchValue = '';
            row.dataset.ehblMatchValueNorm = '';
            row.dataset.ehblMatchReason = '';
        }
    }

    // ========== 过滤模式 ==========

    function applyListFilter() {
        const mode = getFilterMode();
        const lrrMode = getLrrFilterMode();
        const rows = document.querySelectorAll('.ehbl-row-all');

        rows.forEach(row => {
            const isBlacklisted = row.dataset.ehblBlacklisted === '1';

            let ehblHide = false;
            if (mode === 'hide' && isBlacklisted) ehblHide = true;
            else if (mode === 'only' && !isBlacklisted) ehblHide = true;
            row.dataset.ehblHide = ehblHide ? '1' : '0';

            const isDownloaded =
                row.classList.contains('lrr-downloaded') ||
                row.classList.contains('lrr-item-hidden');

            let lrrHide = false;
            if (lrrMode === 'hide' && isDownloaded) lrrHide = true;
            else if (lrrMode === 'only' && !isDownloaded) lrrHide = true;
            row.dataset.lrrHide = lrrHide ? '1' : '0';

            recomputeRowDisplay(row);
        });
    }

    let filterBarBuilt = false;

    function buildFilterBar() {
        if (filterBarBuilt) return;
        filterBarBuilt = true;

        const bar = document.createElement('div');
        bar.id = 'ehbl-filter-bar';

        const label = document.createElement('span');
        label.textContent = '黑名单过滤';

        const tabsWrapper = document.createElement('div');
        tabsWrapper.id = 'ehbl-filter-bar-tabs';

        const modes = [
            { value: 'all', text: '全部' },
            { value: 'hide', text: '隐藏' },
            { value: 'only', text: '只看黑名单' }
        ];

        const current = getFilterMode();

        modes.forEach(m => {
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.textContent = m.text;
            btn.className = 'ehbl-filter-tab';
            if (m.value === current) btn.classList.add('ehbl-filter-tab-active');
            btn.addEventListener('click', () => {
                setFilterMode(m.value);
                document
                    .querySelectorAll('#ehbl-filter-bar-tabs .ehbl-filter-tab')
                    .forEach(el => el.classList.remove('ehbl-filter-tab-active'));
                btn.classList.add('ehbl-filter-tab-active');
            });
            tabsWrapper.appendChild(btn);
        });

        bar.appendChild(label);
        bar.appendChild(tabsWrapper);
        document.body.appendChild(bar);
    }

    // ========== 样式 ==========

    GM_addStyle(`
        .ehbl-badge {
            display: inline-block;
            padding: 0 4px;
            margin-right: 4px;
            border-radius: 3px;
            font-size: 0.8em;
            font-weight: bold;
            background-color: #cc3333;
            color: #fff;
        }

        .ehbl-row-blacklisted {
            background-color: rgba(255, 0, 0, 0.08);
        }
        .ehbl-row-all { }

        #ehbl-overlay {
            position: fixed;
            z-index: 99999;
            inset: 0;
            background: rgba(0, 0, 0, 0.5);
            display: none;
            align-items: center;
            justify-content: center;
        }
        #ehbl-panel {
            min-width: 760px;
            max-width: 88vw;
            max-height: 84vh;
            border-radius: 8px;
            box-shadow: 0 0 10px rgba(0,0,0,0.4);
            padding: 12px;
            display: flex;
            flex-direction: column;
            overflow: hidden;
        }
        #ehbl-panel table { width: 100%; }
        #ehbl-panel th, #ehbl-panel td { padding: 2px 4px; }

        #ehbl-panel-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 8px;
        }
        #ehbl-panel-header h2 {
            margin: 0;
            font-size: 18px;
        }
        #ehbl-panel-close {
            cursor: pointer;
            font-weight: bold;
            padding: 0 6px;
        }
        #ehbl-panel-actions {
            margin: 8px 0 4px 0;
            display: flex;
            gap: 8px;
            flex-wrap: wrap;
        }

        #ehbl-tab-bar {
            margin: 4px 0;
            display: flex;
            gap: 4px;
            font-size: 12px;
        }
        .ehbl-tab-btn {
            padding: 2px 8px;
            border-radius: 4px;
            border: 1px solid #888;
            cursor: pointer;
            flex: 1;
        }
        .ehbl-tab-btn-active {
            background: #006eff;
            font-weight: bold;
        }

        #ehbl-search-bar {
            margin: 4px 0;
            display: flex;
            align-items: center;
            gap: 4px;
            font-size: 12px;
        }
        #ehbl-search-bar input {
            flex: 1;
            padding: 2px 4px;
            font-size: 12px;
        }

        #ehbl-table-wrapper {
            flex: 1 1 auto;
            overflow: auto;
            margin-top: 4px;
        }

        #ehbl-pagination {
            margin-top: 4px;
            display: flex;
            justify-content: space-between;
            align-items: center;
            font-size: 12px;
        }

        #ehbl-import-area { resize: vertical; min-height: 80px; }
        .ehbl-inline-btn {
            padding: 2px 6px;
            font-size: 11px;
            cursor: pointer;
            margin-right: 3px;
        }
        .ehbl-input {
            padding: 2px 4px;
            font-size: 11px;
            width: 100%;
            box-sizing: border-box;
        }
        .ehbl-input-small { width: 64px; }

        #ehbl-floating-manage-btn {
            position: fixed;
            left: 8px;
            bottom: 8px;
            z-index: 9999;
            padding: 4px 8px;
            font-size: 12px;
            border-radius: 4px;
            background: rgba(0,0,0,0.6);
            color: #fff;
            border: none;
            cursor: pointer;
        }
        #ehbl-floating-manage-btn:hover { background: rgba(0,0,0,0.8); }

        #ehbl-filter-bar {
            position: fixed;
            left: 8px;
            bottom: 40px;
            z-index: 9999;
            font-size: 12px;
            background: rgba(0,0,0,0.6);
            color: #fff;
            padding: 4px 6px;
            border-radius: 4px;
        }
        #ehbl-filter-bar-tabs {
            display: inline-flex;
            gap: 4px;
            margin-left: 4px;
        }
        .ehbl-filter-tab {
            padding: 2px 6px;
            font-size: 11px;
            border-radius: 3px;
            border: 1px solid #ccc;
            background: rgba(0,0,0,0.3);
            color: #fff;
            cursor: pointer;
        }
        .ehbl-filter-tab-active {
            background: #006eff;
            border-color: #006eff;
            font-weight: bold;
        }

        .ehbl-thumb-tag{
            position: absolute;
            left: 4px;
            top: 4px;
            padding: 2px 6px;
            background: rgba(204,51,51,0.95);
            color: #fff;
            font-size: 11px;
            border-radius: 4px;
            z-index: 50;
            pointer-events: none;
            line-height: 1.2;
            max-width: 160px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }

        .ehbl-thumb-btn{
            position: absolute;
            right: 4px;
            padding: 2px 6px;
            background: rgba(0,0,0,0.75);
            color: #fff;
            font-size: 11px;
            border-radius: 4px;
            cursor: pointer;
            z-index: 50;
            line-height: 1.2;
            pointer-events: auto;
            user-select: none;
        }
        .ehbl-thumb-btn:hover{ background: rgba(0,0,0,0.9); }
        .ehbl-thumb-btn-id { bottom: 4px; }
        .ehbl-thumb-btn-kw { bottom: 24px; }

        .ehbl-reason-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(110px, 1fr));
            gap: 8px;
            max-height: 260px;
            overflow: auto;
            margin: 8px 0 10px 0;
        }
        .ehbl-reason-choice {
            padding: 7px 10px;
            border: 1px solid #aaa;
            border-radius: 6px;
            background: #fff;
            color: #111;
            cursor: pointer;
            text-align: center;
            font-size: 12px;
            line-height: 1.35;
            word-break: break-all;
        }
        .ehbl-reason-choice:hover {
            background: #e8f0ff;
            border-color: #006eff;
        }
        .ehbl-reason-choice-active {
            background: #006eff;
            border-color: #006eff;
            color: #fff;
            font-weight: bold;
        }
    `);

    // ========== 原因选择 ==========

    function createReasonSelect(selectedCode, selectedLabel) {
        const select = document.createElement('select');
        select.className = 'ehbl-input';

        const emptyOpt = document.createElement('option');
        emptyOpt.value = '';
        emptyOpt.dataset.label = '';
        emptyOpt.textContent = '无原因';
        select.appendChild(emptyOpt);

        const reasons = cachedReasons || [];
        let matched = !selectedCode && !selectedLabel;

        reasons.forEach(r => {
            const opt = document.createElement('option');
            opt.value = r.code || '';
            opt.dataset.label = r.label || '';
            opt.textContent = r.label || r.code || '';
            if ((selectedCode && selectedCode === r.code) || (!selectedCode && selectedLabel && selectedLabel === r.label)) {
                opt.selected = true;
                matched = true;
            }
            select.appendChild(opt);
        });

        if (!matched && selectedLabel) {
            const currentOpt = document.createElement('option');
            currentOpt.value = selectedCode || '__current__';
            currentOpt.dataset.label = selectedLabel;
            currentOpt.textContent = selectedLabel + '（当前）';
            currentOpt.selected = true;
            select.appendChild(currentOpt);
        }

        return select;
    }

    function readReasonSelect(select) {
        const opt = select.options[select.selectedIndex];
        return {
            reason_code: select.value === '__current__' ? '' : (select.value || ''),
            reason_label: opt ? (opt.dataset.label || '') : ''
        };
    }

    async function askReason(selectedCode, selectedLabel, options) {
        options = options || {};
        try {
            await getReasons(false);
        } catch (e) {
            console.warn('[EH-BL API] 获取原因失败', e);
            cachedReasons = cachedReasons || [];
        }

        return new Promise(resolve => {
            const mask = document.createElement('div');
            mask.style.position = 'fixed';
            mask.style.inset = '0';
            mask.style.zIndex = '100000';
            mask.style.background = 'rgba(0,0,0,0.45)';
            mask.style.display = 'flex';
            mask.style.alignItems = 'center';
            mask.style.justifyContent = 'center';

            const box = document.createElement('div');
            box.style.minWidth = '360px';
            box.style.maxWidth = '620px';
            box.style.maxHeight = '78vh';
            box.style.overflow = 'hidden';
            box.style.padding = '12px';
            box.style.borderRadius = '8px';
            box.style.background = '#f7f7f7';
            box.style.color = '#111';
            box.style.boxShadow = '0 2px 12px rgba(0,0,0,0.45)';

            const title = document.createElement('div');
            title.textContent = options.title || '选择黑名单原因（点击即提交）';
            title.style.fontWeight = 'bold';
            title.style.marginBottom = '8px';
            box.appendChild(title);

            let extraInput = null;
            if (options.inputLabel || options.inputDefault != null) {
                const inputWrap = document.createElement('div');
                inputWrap.style.margin = '0 0 10px 0';

                const inputLabel = document.createElement('div');
                inputLabel.textContent = options.inputLabel || '内容';
                inputLabel.style.fontSize = '12px';
                inputLabel.style.fontWeight = 'bold';
                inputLabel.style.marginBottom = '4px';
                inputWrap.appendChild(inputLabel);

                extraInput = document.createElement('input');
                extraInput.type = 'text';
                extraInput.className = 'ehbl-input';
                extraInput.value = options.inputDefault || '';
                extraInput.placeholder = options.inputPlaceholder || '';
                inputWrap.appendChild(extraInput);

                box.appendChild(inputWrap);
            }

            const closeAndResolve = reason => {
                mask.remove();
                resolve(reason);
            };

            const makeReasonButton = (label, code, extraClass) => {
                const btn = document.createElement('button');
                btn.type = 'button';
                btn.className = 'ehbl-reason-choice' + (extraClass ? ' ' + extraClass : '');
                btn.textContent = label || '无原因';
                btn.title = '点击后直接加入黑名单';
                btn.addEventListener('click', () => {
                    const result = {
                        reason_code: code || '',
                        reason_label: label || ''
                    };

                    if (extraInput) {
                        const inputValue = extraInput.value.trim();
                        if (!inputValue) {
                            alert(options.inputRequiredMessage || '内容不能为空');
                            extraInput.focus();
                            return;
                        }
                        result[options.inputResultKey || 'input_value'] = inputValue;
                    }

                    closeAndResolve(result);
                });
                return btn;
            };

            const grid = document.createElement('div');
            grid.className = 'ehbl-reason-grid';

            const noReasonActive = !selectedCode && !selectedLabel;
            grid.appendChild(makeReasonButton('无原因', '', noReasonActive ? 'ehbl-reason-choice-active' : ''));

            const reasons = cachedReasons || [];
            let matched = noReasonActive;
            reasons.forEach(r => {
                const code = r.code || '';
                const label = r.label || r.code || '';
                const active = !!((selectedCode && selectedCode === code) || (!selectedCode && selectedLabel && selectedLabel === label));
                if (active) matched = true;
                grid.appendChild(makeReasonButton(label, code, active ? 'ehbl-reason-choice-active' : ''));
            });

            if (!matched && selectedLabel) {
                grid.appendChild(makeReasonButton(selectedLabel + '（当前）', selectedCode || '', 'ehbl-reason-choice-active'));
            }

            box.appendChild(grid);

            const hint = document.createElement('div');
            hint.textContent = options.hint || '需要新原因时，请先在“黑名单管理 → 常用原因”中新增。';
            hint.style.fontSize = '12px';
            hint.style.opacity = '0.75';
            hint.style.marginBottom = '10px';
            box.appendChild(hint);

            const actions = document.createElement('div');
            actions.style.display = 'flex';
            actions.style.justifyContent = 'flex-end';
            actions.style.gap = '8px';

            const cancelBtn = document.createElement('button');
            cancelBtn.type = 'button';
            cancelBtn.textContent = '取消';
            cancelBtn.className = 'ehbl-inline-btn';
            cancelBtn.addEventListener('click', () => closeAndResolve(null));

            actions.appendChild(cancelBtn);
            box.appendChild(actions);
            mask.appendChild(box);
            document.body.appendChild(mask);

            if (extraInput) {
                extraInput.focus();
                extraInput.select();
            }
        });
    }

    // ========== 管理面板 ==========

    function buildManagerUI() {
        if (overlayEl) return;

        overlayEl = createElement('div', '');
        overlayEl.id = 'ehbl-overlay';

        const panel = createElement('div', '');
        panel.id = 'ehbl-panel';

        try {
            const bodyStyle = window.getComputedStyle(document.body);
            const bgImage = bodyStyle.backgroundImage;
            const bgColor = bodyStyle.backgroundColor;
            const bg = bodyStyle.background;

            if (bgImage && bgImage !== 'none') panel.style.background = bg;
            else if (bgColor && bgColor !== 'rgba(0, 0, 0, 0)' && bgColor !== 'transparent') panel.style.backgroundColor = bgColor;
            else panel.style.backgroundColor = '#fafafa';
        } catch (e) {
            panel.style.backgroundColor = '#fafafa';
        }

        const header = createElement('div', '');
        header.id = 'ehbl-panel-header';
        const title = createElement('h2', '', 'ExHentai 黑名单管理 API版');
        const closeBtn = createElement('span', '', '×');
        closeBtn.id = 'ehbl-panel-close';
        closeBtn.addEventListener('click', () => { overlayEl.style.display = 'none'; });
        header.appendChild(title);
        header.appendChild(closeBtn);

        const actions = createElement('div', '');
        actions.id = 'ehbl-panel-actions';

        const addKwBtn = createElement('button', 'ehbl-inline-btn', '新增关键词');
        addKwBtn.addEventListener('click', addKeywordEntry);

        const addIdBtn = createElement('button', 'ehbl-inline-btn', '新增 ID');
        addIdBtn.addEventListener('click', addIdEntry);

        const addReasonBtn = createElement('button', 'ehbl-inline-btn', '新增常用原因');
        addReasonBtn.addEventListener('click', addReasonEntry);

        const refreshBtn = createElement('button', 'ehbl-inline-btn', '刷新列表');
        refreshBtn.addEventListener('click', () => { renderTable(); refreshCurrentPageMatches(); });

        const settingsBtn = createElement('button', 'ehbl-inline-btn', '设置');
        settingsBtn.addEventListener('click', openSettings);

        const exportBtn = createElement('button', 'ehbl-inline-btn', '导出 JSON 到下方');
        exportBtn.addEventListener('click', async () => {
            try {
                const data = await apiRequest('GET', '/export');
                importTextArea.value = JSON.stringify(data, null, 2);
            } catch (e) {
                showApiError(e, '导出失败');
            }
        });

        const importBtn = createElement('button', 'ehbl-inline-btn', '从下方导入 JSON 并覆盖');
        importBtn.addEventListener('click', async () => {
            if (!confirm('确定用下面 JSON 覆盖当前服务端黑名单吗?')) return;
            try {
                const data = JSON.parse(importTextArea.value || '{}');
                await apiRequest('PUT', '/import?overwrite=true', data);
                cachedReasons = null;
                alert('导入成功');
                currentPage = 1;
                await renderTable();
                await refreshCurrentPageMatches();
            } catch (e) {
                showApiError(e, '导入失败');
            }
        });

        actions.appendChild(addKwBtn);
        actions.appendChild(addIdBtn);
        actions.appendChild(addReasonBtn);
        actions.appendChild(refreshBtn);
        actions.appendChild(settingsBtn);
        actions.appendChild(exportBtn);
        actions.appendChild(importBtn);

        const tabBar = createElement('div', '');
        tabBar.id = 'ehbl-tab-bar';
        tabBtnKw = createElement('button', 'ehbl-tab-btn', '关键词');
        tabBtnId = createElement('button', 'ehbl-tab-btn', 'ID');
        tabBtnReason = createElement('button', 'ehbl-tab-btn', '常用原因');

        tabBtnKw.addEventListener('click', () => { activeTab = 'kw'; currentPage = 1; renderTable(); });
        tabBtnId.addEventListener('click', () => { activeTab = 'id'; currentPage = 1; renderTable(); });
        tabBtnReason.addEventListener('click', () => { activeTab = 'reason'; currentPage = 1; renderTable(); });

        tabBar.appendChild(tabBtnKw);
        tabBar.appendChild(tabBtnId);
        tabBar.appendChild(tabBtnReason);

        const searchBar = createElement('div', '');
        searchBar.id = 'ehbl-search-bar';
        const searchLabel = createElement('span', '', '搜索:');
        searchInput = document.createElement('input');
        searchInput.type = 'text';
        searchInput.placeholder = '按当前类型搜索';
        searchInput.addEventListener('input', () => {
            currentSearchTerm = searchInput.value.trim().toLowerCase();
            currentPage = 1;
            renderTable();
        });
        searchBar.appendChild(searchLabel);
        searchBar.appendChild(searchInput);

        const tableWrapper = createElement('div', '');
        tableWrapper.id = 'ehbl-table-wrapper';

        const table = document.createElement('table');
        const thead = document.createElement('thead');
        const headRow = document.createElement('tr');
        ['类型', '值 / Code', '原因 / Label', '优先级', '操作'].forEach(t => {
            const th = document.createElement('th');
            th.textContent = t;
            headRow.appendChild(th);
        });
        thead.appendChild(headRow);
        table.appendChild(thead);

        panelTableBody = document.createElement('tbody');
        table.appendChild(panelTableBody);
        tableWrapper.appendChild(table);

        const pagination = createElement('div', '');
        pagination.id = 'ehbl-pagination';

        prevPageBtn = createElement('button', 'ehbl-inline-btn', '上一页');
        nextPageBtn = createElement('button', 'ehbl-inline-btn', '下一页');
        pageInfoSpan = createElement('span', '', '');

        prevPageBtn.addEventListener('click', () => { if (currentPage > 1) { currentPage--; renderTable(); } });
        nextPageBtn.addEventListener('click', () => { currentPage++; renderTable(); });

        pagination.appendChild(prevPageBtn);
        pagination.appendChild(pageInfoSpan);
        pagination.appendChild(nextPageBtn);

        importTextArea = document.createElement('textarea');
        importTextArea.id = 'ehbl-import-area';
        importTextArea.placeholder = '导出或粘贴黑名单 JSON 在这里';

        panel.appendChild(header);
        panel.appendChild(actions);
        panel.appendChild(tabBar);
        panel.appendChild(searchBar);
        panel.appendChild(tableWrapper);
        panel.appendChild(pagination);
        panel.appendChild(createElement('div', '', '导入 / 导出区域:'));
        panel.appendChild(importTextArea);

        overlayEl.appendChild(panel);
        document.body.appendChild(overlayEl);
    }

    async function openManager() {
        if (!hasApiConfig()) {
            openSettings();
            return;
        }
        buildManagerUI();
        currentPage = 1;
        overlayEl.style.display = 'flex';
        await renderTable();
    }

    async function renderTable() {
        if (!panelTableBody) return;
        panelTableBody.innerHTML = '';

        if (tabBtnKw && tabBtnId && tabBtnReason) {
            tabBtnKw.classList.toggle('ehbl-tab-btn-active', activeTab === 'kw');
            tabBtnId.classList.toggle('ehbl-tab-btn-active', activeTab === 'id');
            tabBtnReason.classList.toggle('ehbl-tab-btn-active', activeTab === 'reason');
        }
        if (searchInput) {
            searchInput.placeholder = activeTab === 'kw' ? '搜索关键词 / 原因' : activeTab === 'id' ? '搜索 GID / 原因' : '搜索原因';
        }

        try {
            let entries = [];
            if (activeTab === 'reason') {
                const reasons = await getReasons(true);
                entries = reasons.map(r => ({ kind: 'reason', ...r }));
            } else {
                await getReasons(false);
                const type = activeTab === 'kw' ? 'keyword' : 'id';
                const data = await apiRequest('GET', '/entries' + encodeQuery({ type, limit: API_LIST_LIMIT, offset: 0 }));
                entries = ((data && data.items) || []).map(e => ({ kind: 'entry', ...e }));
            }

            const term = currentSearchTerm;
            const filtered = entries.filter(e => {
                if (!term) return true;
                const text = JSON.stringify(e).toLowerCase();
                return text.includes(term);
            });

            renderTableEntries(filtered);
        } catch (e) {
            const tr = document.createElement('tr');
            const td = document.createElement('td');
            td.colSpan = 5;
            td.textContent = '读取 API 失败: ' + (e && e.message ? e.message : e);
            tr.appendChild(td);
            panelTableBody.appendChild(tr);
        }
    }

    function renderTableEntries(entries) {
        panelTableBody.innerHTML = '';

        if (!entries.length) {
            const tr = document.createElement('tr');
            const td = document.createElement('td');
            td.colSpan = 5;
            td.textContent = '当前没有符合条件的项目';
            tr.appendChild(td);
            panelTableBody.appendChild(tr);
            if (pageInfoSpan) pageInfoSpan.textContent = '第 1 / 1 页, 共 0 项';
            if (prevPageBtn) prevPageBtn.disabled = true;
            if (nextPageBtn) nextPageBtn.disabled = true;
            return;
        }

        const totalPages = Math.max(1, Math.ceil(entries.length / PAGE_SIZE));
        if (currentPage > totalPages) currentPage = totalPages;
        if (currentPage < 1) currentPage = 1;

        const startIndex = (currentPage - 1) * PAGE_SIZE;
        const pageEntries = entries.slice(startIndex, startIndex + PAGE_SIZE);

        pageEntries.forEach(e => {
            if (e.kind === 'reason') renderReasonRow(e);
            else renderEntryRow(e);
        });

        if (pageInfoSpan) pageInfoSpan.textContent = `第 ${currentPage} / ${totalPages} 页, 共 ${entries.length} 项`;
        if (prevPageBtn) prevPageBtn.disabled = currentPage <= 1;
        if (nextPageBtn) nextPageBtn.disabled = currentPage >= totalPages;
    }

    function renderEntryRow(e) {
        const tr = document.createElement('tr');
        const tdType = document.createElement('td');
        tdType.textContent = e.type === 'id' ? 'ID' : '关键词';
        tr.appendChild(tdType);

        const tdValue = document.createElement('td');
        const inputValue = createElement('input', 'ehbl-input', null);
        inputValue.value = e.value || '';
        inputValue.disabled = e.type === 'id';
        tdValue.appendChild(inputValue);
        tr.appendChild(tdValue);

        const tdReason = document.createElement('td');
        const reasonSelect = createReasonSelect(e.reason_code || '', e.reason_label || '');
        tdReason.appendChild(reasonSelect);
        tr.appendChild(tdReason);

        const tdPriority = document.createElement('td');
        const inputPriority = createElement('input', 'ehbl-input ehbl-input-small', null);
        inputPriority.type = 'number';
        inputPriority.value = String(e.priority ?? 100);
        tdPriority.appendChild(inputPriority);
        tr.appendChild(tdPriority);

        const tdOps = document.createElement('td');

        const saveBtn = createElement('button', 'ehbl-inline-btn', '保存');
        saveBtn.addEventListener('click', async () => {
            try {
                const value = e.type === 'id' ? e.value : inputValue.value.trim();
                if (!value) { alert('值不能为空'); return; }
                const reason = readReasonSelect(reasonSelect);
                const saved = await upsertEntry({
                    type: e.type,
                    value,
                    reason_code: reason.reason_code,
                    reason_label: reason.reason_label,
                    priority: parseInt(inputPriority.value, 10) || 100
                });

                // 关键词的 value 改变后，后端会因为 value_norm 改变而新增一条记录。
                // 因此这里需要主动删除旧 value_norm，避免“旧关键词还在 + 新关键词多出一条”。
                if (
                    e.type === 'keyword' &&
                    saved &&
                    saved.value_norm &&
                    e.value_norm &&
                    saved.value_norm !== e.value_norm
                ) {
                    await deleteEntry('keyword', e.value_norm);
                }

                alert('已保存');
                await renderTable();
                await refreshCurrentPageMatches();
            } catch (err) {
                showApiError(err, '保存失败');
            }
        });
        tdOps.appendChild(saveBtn);

        const delBtn = createElement('button', 'ehbl-inline-btn', '删除');
        delBtn.addEventListener('click', async () => {
            if (e.type !== 'id' && !confirm('确定删除该黑名单记录吗？')) return;
            try {
                await deleteEntry(e.type, e.value_norm);
                await renderTable();
                await refreshCurrentPageMatches();
            } catch (err) {
                showApiError(err, '删除失败');
            }
        });
        tdOps.appendChild(delBtn);

        tr.appendChild(tdOps);
        panelTableBody.appendChild(tr);
    }

    function renderReasonRow(r) {
        const tr = document.createElement('tr');
        const tdType = document.createElement('td');
        tdType.textContent = r.builtin ? '内置原因' : '常用原因';
        tr.appendChild(tdType);

        const tdCode = document.createElement('td');
        tdCode.textContent = r.code || '';
        tr.appendChild(tdCode);

        const tdLabel = document.createElement('td');
        const inputLabel = createElement('input', 'ehbl-input', null);
        inputLabel.value = r.label || '';
        tdLabel.appendChild(inputLabel);
        tr.appendChild(tdLabel);

        const tdSort = document.createElement('td');
        const inputSort = createElement('input', 'ehbl-input ehbl-input-small', null);
        inputSort.type = 'number';
        inputSort.value = String(r.sort_order ?? 100);
        tdSort.appendChild(inputSort);
        tr.appendChild(tdSort);

        const tdOps = document.createElement('td');
        const saveBtn = createElement('button', 'ehbl-inline-btn', '保存');
        saveBtn.addEventListener('click', async () => {
            try {
                await apiRequest('PUT', '/reasons/' + encodeURIComponent(r.code), {
                    label: inputLabel.value.trim(),
                    sort_order: parseInt(inputSort.value, 10) || 100,
                    builtin: !!r.builtin
                });
                cachedReasons = null;
                alert('已保存');
                await renderTable();
            } catch (err) {
                showApiError(err, '保存原因失败');
            }
        });
        tdOps.appendChild(saveBtn);

        const delBtn = createElement('button', 'ehbl-inline-btn', '删除');
        delBtn.addEventListener('click', async () => {
            if (!confirm('确定删除该常用原因吗？已有黑名单记录的 reason_label 不会被删除。')) return;
            try {
                await apiRequest('DELETE', '/reasons/' + encodeURIComponent(r.code));
                cachedReasons = null;
                await renderTable();
            } catch (err) {
                showApiError(err, '删除原因失败');
            }
        });
        tdOps.appendChild(delBtn);

        tr.appendChild(tdOps);
        panelTableBody.appendChild(tr);
    }

    async function addKeywordEntry(defaultText) {
        const picked = await askReason('', '', {
            title: '加入关键词黑名单（点击原因即提交）',
            inputLabel: '关键词',
            inputDefault: defaultText || '',
            inputPlaceholder: '输入或修改要加入黑名单的关键词',
            inputRequiredMessage: '关键词不能为空',
            inputResultKey: 'value',
            hint: '可以先修改上方关键词，然后点击下面任意原因直接提交。需要新原因时，请先在“黑名单管理 → 常用原因”中新增。'
        });
        if (picked === null) return;

        const value = String(picked.value || '').trim();
        if (!value) { alert('关键词不能为空'); return; }

        try {
            await upsertEntry({
                type: 'keyword',
                value,
                reason_code: picked.reason_code,
                reason_label: picked.reason_label,
                priority: 100
            });
            activeTab = 'kw';
            currentPage = 1;
            await renderTable();
            await refreshCurrentPageMatches();
        } catch (e) {
            showApiError(e, '新增关键词失败');
        }
    }

    async function addIdEntry(defaultGid) {
        let value = String(defaultGid || '').trim();
        if (!value) {
            const input = prompt('请输入要加入黑名单的 GID:', '');
            if (input === null) return;
            value = input.trim();
        }
        if (!/^\d+$/.test(value)) { alert('GID 格式不正确'); return; }

        const reason = await askReason();
        if (reason === null) return;

        try {
            await upsertEntry({ type: 'id', value, ...reason, priority: 100 });
            activeTab = 'id';
            currentPage = 1;
            await renderTable();
            await refreshCurrentPageMatches();
        } catch (e) {
            showApiError(e, '新增 ID 失败');
        }
    }

    async function addReasonEntry() {
        const input = prompt('请输入新的常用原因:');
        if (input === null) return;
        const label = input.trim();
        if (!label) { alert('原因不能为空'); return; }
        try {
            await createReasonIfNeeded(label);
            activeTab = 'reason';
            currentPage = 1;
            await renderTable();
        } catch (e) {
            showApiError(e, '新增常用原因失败');
        }
    }

    // 管理入口按钮
    if (typeof GM_registerMenuCommand === 'function') {
        GM_registerMenuCommand('黑名单管理 API版', openManager);
        GM_registerMenuCommand('黑名单后端设置', openSettings);
    }
    const floatBtn = createElement('button', '', '黑名单管理');
    floatBtn.id = 'ehbl-floating-manage-btn';
    floatBtn.addEventListener('click', openManager);
    document.body.appendChild(floatBtn);

    // ========== 列表页 ==========

    function syncListRowVisual(row, result) {
        const titleEl = row.querySelector('.gl4t.glname.glink');
        if (!titleEl) return;

        const anyMatched = !!(result && result.blacklisted && result.match);
        if (anyMatched) row.classList.add('ehbl-row-blacklisted');
        else row.classList.remove('ehbl-row-blacklisted');

        let badge = titleEl.querySelector('.ehbl-badge');
        if (anyMatched) {
            if (!badge) {
                badge = createElement('span', 'ehbl-badge', '');
                titleEl.prepend(badge);
            }
            badge.textContent = getMatchLabel(result);
            badge.title = getMatchTooltip(result);
        } else if (badge) {
            badge.remove();
        }

        const btnId = row.querySelector('.ehbl-thumb-btn-id');
        const btnKw = row.querySelector('.ehbl-thumb-btn-kw');
        const match = result && result.match ? result.match : null;

        if (btnId) btnId.textContent = match && match.type === 'id' ? '移除 ID 黑名单' : '按 ID 加入黑名单';
        if (btnKw) btnKw.textContent = match && match.type === 'keyword' ? `管理关键词 (${match.reason_label || match.value || '命中'})` : '按关键词加入黑名单';
    }

    function collectListRows() {
        const items = document.querySelectorAll('.itg .gl1t');
        const rows = [];

        items.forEach(item => {
            const thumb = item.querySelector('.gl3t');
            const titleEl = item.querySelector('.gl4t.glname.glink');
            const link = item.querySelector('a[href*="/g/"]');
            if (!thumb || !titleEl || !link) return;

            const href = link.href;
            const gid = getGalleryIdFromUrl(href);
            if (!gid) return;

            if (item.dataset.ehblInit !== '1') {
                item.dataset.ehblInit = '1';
                item.classList.add('ehbl-row-all');
                thumb.style.position = 'relative';

                const btnId = createElement('span', 'ehbl-thumb-btn ehbl-thumb-btn-id', '按 ID 加入黑名单');
                const btnKw = createElement('span', 'ehbl-thumb-btn ehbl-thumb-btn-kw', '按关键词加入黑名单');

                btnId.addEventListener('click', async ev => {
                    ev.stopPropagation();
                    ev.preventDefault();

                    const result = latestListResults.get(gid);
                    if (result && result.match && result.match.type === 'id') {
                        try {
                            await deleteEntry('id', result.match.value_norm || gid);
                            await refreshCurrentPageMatches();
                        } catch (e) {
                            showApiError(e, '移除 ID 失败');
                        }
                    } else {
                        await addIdEntry(gid);
                    }
                });

                btnKw.addEventListener('click', async ev => {
                    ev.stopPropagation();
                    ev.preventDefault();

                    const result = latestListResults.get(gid);
                    if (result && result.match && result.match.type === 'keyword') {
                        buildManagerUI();
                        activeTab = 'kw';
                        currentSearchTerm = result.match.value_norm || result.match.value || '';
                        if (searchInput) searchInput.value = currentSearchTerm;
                        overlayEl.style.display = 'flex';
                        await renderTable();
                    } else {
                        const titleText = item.dataset.ehblTitle || stripOurBadges(titleEl.textContent);
                        await addKeywordEntry(titleText);
                    }
                });

                thumb.appendChild(btnId);
                thumb.appendChild(btnKw);
            }

            const titleText = stripOurBadges(titleEl.textContent);
            item.dataset.ehblGid = gid;
            item.dataset.ehblTitle = titleText;
            rows.push(item);
        });

        return rows;
    }

    async function refreshListMatches() {
        const rows = collectListRows();
        if (!rows.length) return;

        const items = rows.map(row => ({
            gid: row.dataset.ehblGid,
            title: row.dataset.ehblTitle || ''
        }));

        try {
            const results = await checkItems(items);
            latestListResults = new Map();
            results.forEach(result => {
                latestListResults.set(String(result.gid), result);
            });

            rows.forEach(row => {
                const gid = row.dataset.ehblGid;
                const result = latestListResults.get(String(gid)) || { gid, blacklisted: false, match: null };
                applyResultToRowDataset(row, result);
                syncListRowVisual(row, result);
            });
            buildFilterBar();
            applyListFilter();
        } catch (e) {
            console.error('[EH-BL API] 批量查询失败:', e);
            buildFilterBar();
            applyListFilter();
        }
    }

    async function refreshCurrentPageMatches() {
        if (document.querySelector('.itg .gl1t')) await refreshListMatches();
        else await refreshDetailMatch();
    }

    function handleListPage() {
        collectListRows();
        buildFilterBar();
        refreshListMatches();
    }

    // ========== 详情页 ==========

    async function refreshDetailMatch() {
        const gid = getCurrentGalleryId();
        if (!gid) return;

        const titleEl = document.querySelector('#gn') || document.querySelector('#gj');
        if (!titleEl) return;

        const titleText = stripOurBadges(titleEl.textContent);
        try {
            const results = await checkItems([{ gid, title: titleText }]);
            detailResult = results[0] || { gid, blacklisted: false, match: null };
            syncDetailVisual(gid, titleEl, detailResult);
        } catch (e) {
            console.error('[EH-BL API] 详情页查询失败:', e);
        }
    }

    function syncDetailVisual(gid, titleEl, result) {
        let badge = titleEl.querySelector('.ehbl-badge');
        const anyMatched = !!(result && result.blacklisted && result.match);

        if (anyMatched) {
            if (!badge) {
                badge = createElement('span', 'ehbl-badge', '');
                badge.style.marginRight = '8px';
                titleEl.prepend(badge);
            }
            badge.textContent = getMatchLabel(result);
            badge.title = getMatchTooltip(result);
        } else if (badge) {
            badge.remove();
        }

        const thumbContainer = document.querySelector('#gd1') || document.querySelector('#gleft #gd1');
        if (thumbContainer) {
            let tag = thumbContainer.querySelector('.ehbl-thumb-tag');
            if (anyMatched) {
                if (!tag) {
                    tag = createElement('span', 'ehbl-thumb-tag', '');
                    thumbContainer.appendChild(tag);
                }
                tag.textContent = getMatchLabel(result);
                tag.title = getMatchTooltip(result);
            } else if (tag) {
                tag.remove();
            }
        }

        const btnId = document.getElementById('ehbl-detail-btn-id');
        const btnKw = document.getElementById('ehbl-detail-btn-kw');
        const match = result && result.match ? result.match : null;
        if (btnId) btnId.textContent = match && match.type === 'id' ? '移除 ID 黑名单' : '按 ID 加入黑名单';
        if (btnKw) btnKw.textContent = match && match.type === 'keyword' ? `管理关键词 (${match.reason_label || match.value || '命中'})` : '按关键词加入黑名单';
    }

    function handleDetailPage() {
        const gid = getCurrentGalleryId();
        if (!gid) return;

        const titleEl = document.querySelector('#gn') || document.querySelector('#gj');
        if (!titleEl) return;

        const thumbContainer = document.querySelector('#gd1') || document.querySelector('#gleft #gd1');
        if (thumbContainer) {
            const cs = window.getComputedStyle(thumbContainer);
            if (cs.position === 'static') thumbContainer.style.position = 'relative';
        }

        const btnId = createElement('span', 'ehbl-thumb-btn ehbl-thumb-btn-id', '按 ID 加入黑名单');
        btnId.id = 'ehbl-detail-btn-id';
        const btnKw = createElement('span', 'ehbl-thumb-btn ehbl-thumb-btn-kw', '按关键词加入黑名单');
        btnKw.id = 'ehbl-detail-btn-kw';

        btnId.addEventListener('click', async ev => {
            ev.stopPropagation();
            ev.preventDefault();
            if (detailResult && detailResult.match && detailResult.match.type === 'id') {
                try {
                    await deleteEntry('id', detailResult.match.value_norm || gid);
                    await refreshDetailMatch();
                } catch (e) {
                    showApiError(e, '移除 ID 失败');
                }
            } else {
                await addIdEntry(gid);
                await refreshDetailMatch();
            }
        });

        btnKw.addEventListener('click', async ev => {
            ev.stopPropagation();
            ev.preventDefault();
            if (detailResult && detailResult.match && detailResult.match.type === 'keyword') {
                buildManagerUI();
                activeTab = 'kw';
                currentSearchTerm = detailResult.match.value_norm || detailResult.match.value || '';
                if (searchInput) searchInput.value = currentSearchTerm;
                overlayEl.style.display = 'flex';
                await renderTable();
            } else {
                const titleText = stripOurBadges(titleEl.textContent);
                await addKeywordEntry(titleText);
                await refreshDetailMatch();
            }
        });

        if (thumbContainer) {
            thumbContainer.appendChild(btnId);
            thumbContainer.appendChild(btnKw);
        } else if (titleEl.parentElement) {
            titleEl.parentElement.insertBefore(btnKw, titleEl.nextSibling);
            titleEl.parentElement.insertBefore(btnId, btnKw.nextSibling);
        } else {
            titleEl.appendChild(btnId);
            titleEl.appendChild(btnKw);
        }

        refreshDetailMatch();
    }

    // ========== 入口 ==========

    function init() {
        if (!hasApiConfig()) {
            console.warn('[EH-BL API] 未配置后端地址，已暂停查询');
            showConfigBanner();
            return;
        }
        hideConfigBanner();
        pageInitialized = true;
        const hasList = document.querySelector('.itg .gl1t');
        if (hasList) handleListPage();
        else handleDetailPage();
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
