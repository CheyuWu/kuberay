(function() {
    let clusters = [];
    let entering = null;

    const errorContainer = document.getElementById('error-container');
    const searchInput = document.getElementById('search-input');
    const loadingContainer = document.getElementById('loading-container');
    const tableContainer = document.getElementById('table-container');
    const tbody = document.getElementById('clusters-tbody');
    const showingText = document.getElementById('showing-text');

    function showError(message) {
        if (message) {
            errorContainer.textContent = message;
            errorContainer.classList.remove('hidden');
        } else {
            errorContainer.classList.add('hidden');
        }
    }

    function fetchClusters() {
        showError(null);
        loadingContainer.classList.remove('hidden');
        tableContainer.classList.add('hidden');

        fetch('/clusters')
            .then(res => {
                if (!res.ok) {
                    throw new Error(`${res.status}: ${res.statusText || 'Error'}`);
                }
                return res.json();
            })
            .then(data => {
                clusters = data || [];
                renderTable();
                loadingContainer.classList.add('hidden');
                tableContainer.classList.remove('hidden');
            })
            .catch(err => {
                showError(err.message || "500: Internal Server Error");
                loadingContainer.classList.add('hidden');
            });
    }

    function enterCluster(namespace, name, session) {
        if (entering) return; // In-flight lock
        entering = `${namespace}/${name}/${session}`;
        showError(null);
        renderTable();

        fetch(`/enter_cluster/${namespace}/raycluster/${name}/${session}`)
            .then(res => {
                if (!res.ok) {
                    return res.text().then(body => {
                        throw new Error(body.trim() || `${res.status}: ${res.statusText || 'Error'}`);
                    });
                }
                return res.json();
            })
            .then(data => {
                if (data.result === 'success') {
                    window.location.href = '/#/overview';
                } else {
                    showError(data.message || "500: Error entering cluster session");
                    entering = null;
                    renderTable();
                }
            })
            .catch(err => {
                showError(err.message || "500: Communication Error");
                entering = null;
                renderTable();
            });
    }

    function renderTable() {
        const query = searchInput.value.toLowerCase();
        const filtered = clusters.filter(c => {
            return (c.name || '').toLowerCase().includes(query) ||
                   (c.namespace || '').toLowerCase().includes(query) ||
                   (c.sessionName || '').toLowerCase().includes(query) ||
                   (c.ownerName || '').toLowerCase().includes(query);
        });

        tbody.replaceChildren();

        if (filtered.length === 0) {
            const tr = document.createElement('tr');
            const td = document.createElement('td');
            td.setAttribute('colspan', '6');
            td.className = 'empty-state';

            const wrapper = document.createElement('div');
            wrapper.className = 'empty-state-wrapper';

            const svgString = '<svg class="empty-state-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M20 13V6a2 2 0 00-2-2H6a2 2 0 00-2 2v7m16 0v5a2 2 0 01-2 2H6a2 2 0 01-2-2v-5m16 0h-2.586a1 1 0 00-.707.293l-2.414 2.414a1 1 0 01-.707.293h-3.172a1 1 0 01-.707-.293l-2.414-2.414A1 1 0 006.586 13H4" /></svg>';
            const parser = new DOMParser();
            const svgDoc = parser.parseFromString(svgString, 'image/svg+xml');
            wrapper.appendChild(svgDoc.documentElement);

            const titleSpan = document.createElement('span');
            titleSpan.className = 'empty-state-title';
            titleSpan.textContent = 'No cluster sessions found';
            wrapper.appendChild(titleSpan);

            const subtitleSpan = document.createElement('span');
            subtitleSpan.className = 'empty-state-subtitle';
            subtitleSpan.textContent = `We couldn't find any session matching your query "${searchInput.value}".`;
            wrapper.appendChild(subtitleSpan);

            td.appendChild(wrapper);
            tr.appendChild(td);
            tbody.appendChild(tr);
        } else {
            filtered.forEach(c => {
                const isLive = c.sessionName === 'live';
                const clusterId = `${c.namespace}/${c.name}/${c.sessionName}`;
                const isThisEntering = entering === clusterId;
                const isAnyEntering = Boolean(entering);

                const tr = document.createElement('tr');
                tr.className = isAnyEntering ? 'opacity-70' : 'hover-row';

                // Cluster Name
                const tdName = document.createElement('td');
                tdName.className = 'px-6 font-medium text-white break-words';
                const nameWrapper = document.createElement('div');
                nameWrapper.className = 'items-start gap-2.5';
                const dot = document.createElement('div');
                dot.className = `status-dot ${isLive ? 'dot-emerald animate-pulse' : 'dot-blue'}`;
                nameWrapper.appendChild(dot);
                const nameSpan = document.createElement('span');
                nameSpan.className = `cluster-name-link ${isAnyEntering ? 'disabled-link' : 'clickable-link'}`;
                nameSpan.textContent = c.name;
                if (!isAnyEntering) {
                    nameSpan.onclick = () => enterCluster(c.namespace, c.name, c.sessionName);
                }
                nameWrapper.appendChild(nameSpan);
                tdName.appendChild(nameWrapper);
                tr.appendChild(tdName);

                // Namespace
                const tdNamespace = document.createElement('td');
                tdNamespace.className = 'px-6 font-mono text-xs break-words';
                const nsSpan = document.createElement('span');
                nsSpan.className = 'namespace-badge';
                nsSpan.textContent = c.namespace;
                tdNamespace.appendChild(nsSpan);
                tr.appendChild(tdNamespace);

                // Session Status
                const tdStatus = document.createElement('td');
                tdStatus.className = 'px-6 font-mono text-xs break-words';
                const statusBadge = document.createElement('span');
                statusBadge.className = isLive ? 'badge-emerald font-mono' : 'badge-amber font-mono';

                const smallDot = document.createElement('span');
                smallDot.className = isLive ? 'small-dot bg-emerald' : 'small-dot bg-amber';
                statusBadge.appendChild(smallDot);

                const textSpan = document.createElement('span');
                textSpan.textContent = isLive ? 'Live' : (c.sessionName || 'Replay');
                statusBadge.appendChild(textSpan);

                tdStatus.appendChild(statusBadge);
                tr.appendChild(tdStatus);

                // Owner
                const tdOwner = document.createElement('td');
                tdOwner.className = 'px-6 text-xs text-slate-300 break-words';
                if (c.ownerKind || c.ownerName) {
                    const ownerWrapper = document.createElement('div');
                    ownerWrapper.className = 'items-start gap-1.5 break-words';
                    if (c.ownerKind) {
                        const kindBadge = document.createElement('span');
                        kindBadge.className = 'badge-indigo';
                        kindBadge.textContent = c.ownerKind;
                        ownerWrapper.appendChild(kindBadge);
                    }
                    const ownerNameSpan = document.createElement('span');
                    ownerNameSpan.className = 'owner-name';
                    ownerNameSpan.textContent = c.ownerName || '—';
                    ownerWrapper.appendChild(ownerNameSpan);
                    tdOwner.appendChild(ownerWrapper);
                } else {
                    const dashSpan = document.createElement('span');
                    dashSpan.className = 'text-slate-500';
                    dashSpan.textContent = '—';
                    tdOwner.appendChild(dashSpan);
                }
                tr.appendChild(tdOwner);

                // Created At
                const tdCreated = document.createElement('td');
                tdCreated.className = 'px-6 text-xs text-slate-400 font-mono break-words';
                tdCreated.textContent = c.createTime || '—';
                tr.appendChild(tdCreated);

                // Action
                const tdAction = document.createElement('td');
                tdAction.className = 'px-6 text-right';
                const btn = document.createElement('button');
                btn.disabled = isAnyEntering;

                if (isThisEntering) {
                    btn.className = 'btn btn-wait';
                    const wrapper = document.createElement('div');
                    wrapper.className = 'flex items-center gap-1.5 justify-center w-full';

                    const spinner = document.createElement('div');
                    spinner.className = 'mini-spinner';
                    wrapper.appendChild(spinner);

                    const textSpan = document.createElement('span');
                    textSpan.className = 'whitespace-nowrap';
                    textSpan.textContent = 'Opening...';
                    wrapper.appendChild(textSpan);

                    btn.appendChild(wrapper);
                } else {
                    btn.className = isAnyEntering ? 'btn btn-disabled' : 'btn btn-primary';
                    const wrapper = document.createElement('div');
                    wrapper.className = 'flex items-center gap-1.5 justify-center w-full';

                    const textSpan = document.createElement('span');
                    textSpan.className = 'whitespace-nowrap';
                    textSpan.textContent = 'Open Dashboard';
                    wrapper.appendChild(textSpan);

                    const parser = new DOMParser();
                    const svgDoc = parser.parseFromString('<svg class="arrow-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M14 5l7 7m0 0l-7 7m7-7H3"/></svg>', 'image/svg+xml');
                    wrapper.appendChild(svgDoc.documentElement);

                    btn.appendChild(wrapper);
                    if (!isAnyEntering) {
                        btn.onclick = () => enterCluster(c.namespace, c.name, c.sessionName);
                    }
                }
                tdAction.appendChild(btn);
                tr.appendChild(tdAction);

                tbody.appendChild(tr);
            });
        }

        showingText.textContent = `Showing ${filtered.length} of ${clusters.length} available sessions`;
    }

    searchInput.oninput = () => renderTable();

    window.addEventListener('pageshow', (event) => {
        if (event.persisted || entering !== null) {
            entering = null;
            renderTable();
        }
    });

    // Initial fetch
    fetchClusters();
})();
