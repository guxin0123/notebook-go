(() => {
    "use strict";

    const textarea = document.getElementById("content");
    const statusElement = document.getElementById("save-status");
    const downloadLink = document.querySelector('a[download]');
    if (!textarea || !statusElement) {
        return;
    }

    const noteId = textarea.dataset.noteId || "";
    const noteURL = window.location.pathname;
    const downloadURL = noteURL.replace(/\/+$/, "") + "/download";
    const baseTitle = noteId ? `${noteId} · 记事本` : "记事本";
    const timeFormatter = new Intl.DateTimeFormat("zh-CN", {
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
    });

    let savedValue = textarea.value;
    let saving = false;
    let saveQueued = false;
    let downloadRequested = false;
    let debounceTimer = null;
    let maxWaitTimer = null;
    let retryTimer = null;
    let retryDelay = 1000;

    class SaveError extends Error {
        constructor(message, retryable, status) {
            super(message);
            this.name = "SaveError";
            this.retryable = retryable;
            this.status = status;
        }
    }

    function updateTitle() {
        const dirty = textarea.value !== savedValue;
        document.title = (dirty ? "● " : "") + baseTitle;
    }

    function setStatus(state, text, title) {
        statusElement.dataset.state = state;
        statusElement.textContent = text;
        if (title) {
            statusElement.title = title;
        } else {
            statusElement.removeAttribute("title");
        }
        updateTitle();
    }

    function clearTimer(name) {
        if (name !== null) {
            window.clearTimeout(name);
        }
        return null;
    }

    function scheduleSave(delay) {
        debounceTimer = clearTimer(debounceTimer);
        maxWaitTimer = clearTimer(maxWaitTimer);
        debounceTimer = window.setTimeout(() => {
            debounceTimer = null;
            void saveNow();
        }, delay);
    }

    function scheduleRetry() {
        retryTimer = clearTimer(retryTimer);
        const delay = retryDelay;
        retryDelay = Math.min(retryDelay * 2, 30000);
        retryTimer = window.setTimeout(() => {
            retryTimer = null;
            if (textarea.value !== savedValue) {
                void saveNow();
            }
        }, delay);
    }

    function savedLabel() {
        return "已保存 " + timeFormatter.format(new Date());
    }

    function maybeStartDownload() {
        if (!downloadRequested || saving) {
            return;
        }
        if (textarea.value !== savedValue) {
            return;
        }
        downloadRequested = false;
        const link = document.createElement("a");
        link.href = downloadURL;
        link.download = noteId + ".txt";
        link.hidden = true;
        document.body.appendChild(link);
        link.click();
        link.remove();
    }

    async function saveNow() {
        debounceTimer = clearTimer(debounceTimer);
        maxWaitTimer = clearTimer(maxWaitTimer);
        retryTimer = clearTimer(retryTimer);

        if (saving) {
            saveQueued = true;
            return;
        }
        if (textarea.value === savedValue) {
            setStatus("saved", savedLabel(), "文档已保存到服务器");
            maybeStartDownload();
            return;
        }

        const value = textarea.value;
        saving = true;
        setStatus("saving", downloadRequested ? "保存后下载…" : "保存中…");

        let succeeded = false;
        try {
            const response = await fetch(noteURL, {
                method: "PUT",
                headers: { "Content-Type": "text/plain;charset=UTF-8" },
                body: value,
                cache: "no-store",
                credentials: "same-origin",
            });

            if (!response.ok) {
                let message = `服务器返回 HTTP ${response.status}`;
                let retryable = response.status === 429 || response.status >= 500;
                if (response.status === 401 || response.status === 403) {
                    message = "没有保存权限，可能需要重新登录";
                } else if (response.status === 413) {
                    message = "文档超过服务器允许的大小";
                } else if (response.status === 415) {
                    message = "服务器拒绝了请求格式";
                }
                throw new SaveError(message, retryable, response.status);
            }

            savedValue = value;
            retryDelay = 1000;
            succeeded = true;

            if (textarea.value === savedValue) {
                setStatus("saved", savedLabel(), "文档已保存到服务器");
            } else {
                setStatus("dirty", "未保存");
                saveQueued = true;
            }
        } catch (error) {
            downloadRequested = false;
            const retryable = !(error instanceof SaveError) || error.retryable;
            const detail = error && error.message ? error.message : "网络连接失败";

            if (error instanceof SaveError && error.status === 413) {
                setStatus("error", "文档过大，未保存", detail);
            } else if (error instanceof SaveError && (error.status === 401 || error.status === 403)) {
                setStatus("error", "无保存权限", detail);
            } else if (retryable) {
                setStatus("error", "保存失败，重试中…", detail);
            } else {
                setStatus("error", "保存失败", detail);
            }

            if (retryable) {
                scheduleRetry();
            } else {
                retryTimer = clearTimer(retryTimer);
            }
        } finally {
            saving = false;
            if (succeeded) {
                maybeStartDownload();
            }
            if (saveQueued) {
                saveQueued = false;
                scheduleSave(0);
            }
        }
    }

    function onInput() {
        retryDelay = 1000;
        retryTimer = clearTimer(retryTimer);

        if (textarea.value === savedValue) {
            debounceTimer = clearTimer(debounceTimer);
            maxWaitTimer = clearTimer(maxWaitTimer);
            setStatus("saved", savedLabel(), "文档已保存到服务器");
            return;
        }

        setStatus("dirty", "未保存");
        debounceTimer = clearTimer(debounceTimer);
        debounceTimer = window.setTimeout(() => {
            debounceTimer = null;
            void saveNow();
        }, 700);

        if (maxWaitTimer === null) {
            maxWaitTimer = window.setTimeout(() => {
                maxWaitTimer = null;
                void saveNow();
            }, 5000);
        }
    }

    function flushOnLeave() {
        if (textarea.value === savedValue) {
            return;
        }

        const blob = new Blob([textarea.value], { type: "text/plain;charset=UTF-8" });
        let queued = false;
        if (navigator.sendBeacon) {
            try {
                queued = navigator.sendBeacon(noteURL, blob);
            } catch (_) {
                queued = false;
            }
        }
        if (!queued) {
            try {
                fetch(noteURL, {
                    method: "POST",
                    headers: { "Content-Type": "text/plain;charset=UTF-8" },
                    body: textarea.value,
                    cache: "no-store",
                    credentials: "same-origin",
                    keepalive: true,
                }).catch(() => {});
            } catch (_) {
                // The browser may reject a keepalive body that is too large.
            }
        }
    }

    textarea.addEventListener("input", onInput);
    textarea.addEventListener("blur", () => {
        if (textarea.value !== savedValue) {
            void saveNow();
        }
    });

    document.addEventListener("keydown", (event) => {
        if (event.isComposing || event.altKey) {
            return;
        }
        if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") {
            event.preventDefault();
            retryDelay = 1000;
            void saveNow();
        }
    });

    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") {
            flushOnLeave();
        } else if (textarea.value !== savedValue && !saving) {
            void saveNow();
        }
    });

    window.addEventListener("pagehide", flushOnLeave);

    window.addEventListener("online", () => {
        if (textarea.value !== savedValue && !saving) {
            retryDelay = 1000;
            void saveNow();
        }
    });

    window.addEventListener("offline", () => {
        if (textarea.value !== savedValue) {
            setStatus("error", "网络已断开", "网络恢复后会自动重试");
        }
    });

    if (downloadLink) {
        downloadLink.addEventListener("click", (event) => {
            if (textarea.value === savedValue) {
                return;
            }
            event.preventDefault();
            downloadRequested = true;
            retryDelay = 1000;
            void saveNow();
        });
    }

    setStatus("saved", "已保存", "文档会自动保存");
})();
