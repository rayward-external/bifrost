"use strict";
// While the public API was clearly inspired by the "history" npm package,
// This implementation attempts to be more lightweight by
// making assumptions about the way TanStack Router works
Object.defineProperty(exports, "__esModule", { value: true });
exports.createHistory = createHistory;
exports.createBrowserHistory = createBrowserHistory;
exports.createHashHistory = createHashHistory;
exports.createMemoryHistory = createMemoryHistory;
exports.parseHref = parseHref;
const stateIndexKey = '__TSR_index';
const popStateEvent = 'popstate';
const beforeUnloadEvent = 'beforeunload';
function createHistory(opts) {
    let location = opts.getLocation();
    const subscribers = new Set();
    const notify = (action) => {
        location = opts.getLocation();
        subscribers.forEach((subscriber) => subscriber({ location, action }));
    };
    const handleIndexChange = (action) => {
        if (opts.notifyOnIndexChange ?? true)
            notify(action);
        else
            location = opts.getLocation();
    };
    const tryNavigation = async ({ task, navigateOpts, ...actionInfo }) => {
        const ignoreBlocker = navigateOpts?.ignoreBlocker ?? false;
        if (ignoreBlocker) {
            task();
            return;
        }
        const blockers = opts.getBlockers?.() ?? [];
        const isPushOrReplace = actionInfo.type === 'PUSH' || actionInfo.type === 'REPLACE';
        if (typeof document !== 'undefined' && blockers.length && isPushOrReplace) {
            for (const blocker of blockers) {
                const nextLocation = parseHref(actionInfo.path, actionInfo.state);
                const isBlocked = await blocker.blockerFn({
                    currentLocation: location,
                    nextLocation,
                    action: actionInfo.type,
                });
                if (isBlocked) {
                    opts.onBlocked?.();
                    return;
                }
            }
        }
        task();
    };
    return {
        get location() {
            return location;
        },
        get length() {
            return opts.getLength();
        },
        subscribers,
        subscribe: (cb) => {
            subscribers.add(cb);
            return () => {
                subscribers.delete(cb);
            };
        },
        push: (path, state, navigateOpts) => {
            const currentIndex = location.state[stateIndexKey];
            state = assignKeyAndIndex(currentIndex + 1, state);
            tryNavigation({
                task: () => {
                    opts.pushState(path, state);
                    notify({ type: 'PUSH' });
                },
                navigateOpts,
                type: 'PUSH',
                path,
                state,
            });
        },
        replace: (path, state, navigateOpts) => {
            const currentIndex = location.state[stateIndexKey];
            state = assignKeyAndIndex(currentIndex, state);
            tryNavigation({
                task: () => {
                    opts.replaceState(path, state);
                    notify({ type: 'REPLACE' });
                },
                navigateOpts,
                type: 'REPLACE',
                path,
                state,
            });
        },
        go: (index, navigateOpts) => {
            tryNavigation({
                task: () => {
                    opts.go(index);
                    handleIndexChange({ type: 'GO', index });
                },
                navigateOpts,
                type: 'GO',
            });
        },
        back: (navigateOpts) => {
            tryNavigation({
                task: () => {
                    opts.back(navigateOpts?.ignoreBlocker ?? false);
                    handleIndexChange({ type: 'BACK' });
                },
                navigateOpts,
                type: 'BACK',
            });
        },
        forward: (navigateOpts) => {
            tryNavigation({
                task: () => {
                    opts.forward(navigateOpts?.ignoreBlocker ?? false);
                    handleIndexChange({ type: 'FORWARD' });
                },
                navigateOpts,
                type: 'FORWARD',
            });
        },
        canGoBack: () => location.state[stateIndexKey] !== 0,
        createHref: (str) => opts.createHref(str),
        block: (blocker) => {
            if (!opts.setBlockers)
                return () => { };
            const blockers = opts.getBlockers?.() ?? [];
            opts.setBlockers([...blockers, blocker]);
            return () => {
                const blockers = opts.getBlockers?.() ?? [];
                opts.setBlockers?.(blockers.filter((b) => b !== blocker));
            };
        },
        flush: () => opts.flush?.(),
        destroy: () => opts.destroy?.(),
        notify,
    };
}
function assignKeyAndIndex(index, state) {
    if (!state) {
        state = {};
    }
    const key = createRandomKey();
    return {
        ...state,
        key, // TODO: Remove in v2 - use __TSR_key instead
        __TSR_key: key,
        [stateIndexKey]: index,
    };
}
/**
 * Creates a history object that can be used to interact with the browser's
 * navigation. This is a lightweight API wrapping the browser's native methods.
 * It is designed to work with TanStack Router, but could be used as a standalone API as well.
 * IMPORTANT: This API implements history throttling via a microtask to prevent
 * excessive calls to the history API. In some browsers, calling history.pushState or
 * history.replaceState in quick succession can cause the browser to ignore subsequent
 * calls. This API smooths out those differences and ensures that your application
 * state will *eventually* match the browser state. In most cases, this is not a problem,
 * but if you need to ensure that the browser state is up to date, you can use the
 * `history.flush` method to immediately flush all pending state changes to the browser URL.
 * @param opts
 * @param opts.getHref A function that returns the current href (path + search + hash)
 * @param opts.createHref A function that takes a path and returns a href (path + search + hash)
 * @returns A history instance
 */
function createBrowserHistory(opts) {
    const win = opts?.window ??
        (typeof document !== 'undefined' ? window : undefined);
    const originalPushState = win.history.pushState;
    const originalReplaceState = win.history.replaceState;
    let blockers = [];
    const _getBlockers = () => blockers;
    const _setBlockers = (newBlockers) => (blockers = newBlockers);
    const createHref = opts?.createHref ?? ((path) => path);
    const parseLocation = opts?.parseLocation ??
        (() => parseHref(`${win.location.pathname}${win.location.search}${win.location.hash}`, win.history.state));
    // Ensure there is always a key to start
    if (!win.history.state?.__TSR_key && !win.history.state?.key) {
        const addedKey = createRandomKey();
        win.history.replaceState({
            [stateIndexKey]: 0,
            key: addedKey, // TODO: Remove in v2 - use __TSR_key instead
            __TSR_key: addedKey,
        }, '');
    }
    let currentLocation = parseLocation();
    let rollbackLocation;
    let nextPopIsGo = false;
    let ignoreNextPop = false;
    let skipBlockerNextPop = false;
    let ignoreNextBeforeUnload = false;
    const getLocation = () => currentLocation;
    let next;
    // We need to track the current scheduled update to prevent
    // multiple updates from being scheduled at the same time.
    let scheduled;
    // This function flushes the next update to the browser history
    const flush = () => {
        if (!next) {
            return;
        }
        // We need to ignore any updates to the subscribers while we update the browser history
        history._ignoreSubscribers = true;
        (next.isPush ? win.history.pushState : win.history.replaceState)(next.state, '', next.href);
        // Stop ignoring subscriber updates
        history._ignoreSubscribers = false;
        // Reset the nextIsPush flag and clear the scheduled update
        next = undefined;
        scheduled = undefined;
        rollbackLocation = undefined;
    };
    // This function queues up a call to update the browser history
    const queueHistoryAction = (type, destHref, state) => {
        const href = createHref(destHref);
        if (!scheduled) {
            rollbackLocation = currentLocation;
        }
        // Update the location in memory
        currentLocation = parseHref(destHref, state);
        // Keep track of the next location we need to flush to the URL
        next = {
            href,
            state,
            isPush: next?.isPush || type === 'push',
        };
        if (!scheduled) {
            // Schedule an update to the browser history
            scheduled = Promise.resolve().then(() => flush());
        }
    };
    // NOTE: this function can probably be removed
    const onPushPop = (type) => {
        currentLocation = parseLocation();
        history.notify({ type });
    };
    const onPushPopEvent = async () => {
        if (ignoreNextPop) {
            ignoreNextPop = false;
            return;
        }
        const nextLocation = parseLocation();
        const delta = nextLocation.state[stateIndexKey] - currentLocation.state[stateIndexKey];
        const isForward = delta === 1;
        const isBack = delta === -1;
        const isGo = (!isForward && !isBack) || nextPopIsGo;
        nextPopIsGo = false;
        const action = isGo ? 'GO' : isBack ? 'BACK' : 'FORWARD';
        const notify = isGo
            ? {
                type: 'GO',
                index: delta,
            }
            : {
                type: isBack ? 'BACK' : 'FORWARD',
            };
        if (skipBlockerNextPop) {
            skipBlockerNextPop = false;
        }
        else {
            const blockers = _getBlockers();
            if (typeof document !== 'undefined' && blockers.length) {
                for (const blocker of blockers) {
                    const isBlocked = await blocker.blockerFn({
                        currentLocation,
                        nextLocation,
                        action,
                    });
                    if (isBlocked) {
                        ignoreNextPop = true;
                        win.history.go(1);
                        history.notify(notify);
                        return;
                    }
                }
            }
        }
        currentLocation = parseLocation();
        history.notify(notify);
    };
    const onBeforeUnload = (e) => {
        if (ignoreNextBeforeUnload) {
            ignoreNextBeforeUnload = false;
            return;
        }
        let shouldBlock = false;
        // If one blocker has a non-disabled beforeUnload, we should block
        const blockers = _getBlockers();
        if (typeof document !== 'undefined' && blockers.length) {
            for (const blocker of blockers) {
                const shouldHaveBeforeUnload = blocker.enableBeforeUnload ?? true;
                if (shouldHaveBeforeUnload === true) {
                    shouldBlock = true;
                    break;
                }
                if (typeof shouldHaveBeforeUnload === 'function' &&
                    shouldHaveBeforeUnload() === true) {
                    shouldBlock = true;
                    break;
                }
            }
        }
        if (shouldBlock) {
            e.preventDefault();
            return (e.returnValue = '');
        }
        return;
    };
    const history = createHistory({
        getLocation,
        getLength: () => win.history.length,
        pushState: (href, state) => queueHistoryAction('push', href, state),
        replaceState: (href, state) => queueHistoryAction('replace', href, state),
        back: (ignoreBlocker) => {
            if (ignoreBlocker)
                skipBlockerNextPop = true;
            ignoreNextBeforeUnload = true;
            return win.history.back();
        },
        forward: (ignoreBlocker) => {
            if (ignoreBlocker)
                skipBlockerNextPop = true;
            ignoreNextBeforeUnload = true;
            win.history.forward();
        },
        go: (n) => {
            nextPopIsGo = true;
            win.history.go(n);
        },
        createHref: (href) => createHref(href),
        flush,
        destroy: () => {
            win.history.pushState = originalPushState;
            win.history.replaceState = originalReplaceState;
            win.removeEventListener(beforeUnloadEvent, onBeforeUnload, {
                capture: true,
            });
            win.removeEventListener(popStateEvent, onPushPopEvent);
        },
        onBlocked: () => {
            // If a navigation is blocked, we need to rollback the location
            // that we optimistically updated in memory.
            if (rollbackLocation && currentLocation !== rollbackLocation) {
                currentLocation = rollbackLocation;
            }
        },
        getBlockers: _getBlockers,
        setBlockers: _setBlockers,
        notifyOnIndexChange: false,
    });
    win.addEventListener(beforeUnloadEvent, onBeforeUnload, { capture: true });
    win.addEventListener(popStateEvent, onPushPopEvent);
    win.history.pushState = function (...args) {
        const res = originalPushState.apply(win.history, args);
        if (!history._ignoreSubscribers)
            onPushPop('PUSH');
        return res;
    };
    win.history.replaceState = function (...args) {
        const res = originalReplaceState.apply(win.history, args);
        if (!history._ignoreSubscribers)
            onPushPop('REPLACE');
        return res;
    };
    return history;
}
/**
 * Create a hash-based history implementation.
 * Useful for static hosts or environments without server URL rewriting.
 * @link https://tanstack.com/router/latest/docs/framework/react/guide/history-types
 */
function createHashHistory(opts) {
    const win = opts?.window ??
        (typeof document !== 'undefined' ? window : undefined);
    return createBrowserHistory({
        window: win,
        parseLocation: () => {
            const hashSplit = win.location.hash.split('#').slice(1);
            const pathPart = hashSplit[0] ?? '/';
            const searchPart = win.location.search;
            const hashEntries = hashSplit.slice(1);
            const hashPart = hashEntries.length === 0 ? '' : `#${hashEntries.join('#')}`;
            const hashHref = `${pathPart}${searchPart}${hashPart}`;
            return parseHref(hashHref, win.history.state);
        },
        createHref: (href) => `${win.location.pathname}${win.location.search}#${href}`,
    });
}
/**
 * Create an in-memory history implementation.
 * Ideal for server rendering, tests, and non-DOM environments.
 * @link https://tanstack.com/router/latest/docs/framework/react/guide/history-types
 */
function createMemoryHistory(opts = {
    initialEntries: ['/'],
}) {
    const entries = opts.initialEntries;
    let index = opts.initialIndex
        ? Math.min(Math.max(opts.initialIndex, 0), entries.length - 1)
        : entries.length - 1;
    const states = entries.map((_entry, index) => assignKeyAndIndex(index, undefined));
    const getLocation = () => parseHref(entries[index], states[index]);
    let blockers = [];
    const _getBlockers = () => blockers;
    const _setBlockers = (newBlockers) => (blockers = newBlockers);
    return createHistory({
        getLocation,
        getLength: () => entries.length,
        pushState: (path, state) => {
            // Removes all subsequent entries after the current index to start a new branch
            if (index < entries.length - 1) {
                entries.splice(index + 1);
                states.splice(index + 1);
            }
            states.push(state);
            entries.push(path);
            index = Math.max(entries.length - 1, 0);
        },
        replaceState: (path, state) => {
            states[index] = state;
            entries[index] = path;
        },
        back: () => {
            index = Math.max(index - 1, 0);
        },
        forward: () => {
            index = Math.min(index + 1, entries.length - 1);
        },
        go: (n) => {
            index = Math.min(Math.max(index + n, 0), entries.length - 1);
        },
        createHref: (path) => path,
        getBlockers: _getBlockers,
        setBlockers: _setBlockers,
    });
}
/**
 * Sanitize a path to prevent open redirect vulnerabilities.
 * Removes control characters and collapses leading double slashes.
 */
function sanitizePath(path) {
    // Remove ASCII control characters (0x00-0x1F) and DEL (0x7F)
    // These include CR (\r = 0x0D), LF (\n = 0x0A), and other potentially dangerous characters
    // eslint-disable-next-line no-control-regex
    let sanitized = path.replace(/[\x00-\x1f\x7f]/g, '');
    // Prevent open redirect via protocol-relative URLs (e.g. "//evil.com")
    // Collapse leading double slashes to a single slash
    if (sanitized.startsWith('//')) {
        sanitized = '/' + sanitized.replace(/^\/+/, '');
    }
    return sanitized;
}
function parseHref(href, state) {
    const sanitizedHref = sanitizePath(href);
    const hashIndex = sanitizedHref.indexOf('#');
    const searchIndex = sanitizedHref.indexOf('?');
    const addedKey = createRandomKey();
    return {
        href: sanitizedHref,
        pathname: sanitizedHref.substring(0, hashIndex > 0
            ? searchIndex > 0
                ? Math.min(hashIndex, searchIndex)
                : hashIndex
            : searchIndex > 0
                ? searchIndex
                : sanitizedHref.length),
        hash: hashIndex > -1 ? sanitizedHref.substring(hashIndex) : '',
        search: searchIndex > -1
            ? sanitizedHref.slice(searchIndex, hashIndex === -1 ? undefined : hashIndex)
            : '',
        state: state || { [stateIndexKey]: 0, key: addedKey, __TSR_key: addedKey },
    };
}
// Thanks co-pilot!
function createRandomKey() {
    return (Math.random() + 1).toString(36).substring(7);
}
