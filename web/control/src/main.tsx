import React from "react";
import ReactDOM from "react-dom/client";
import { Panel, PanelGroup, PanelResizeHandle } from "react-resizable-panels";
import {
  ChevronLeft,
  ChevronRight,
  Github,
  Globe,
  LayoutGrid,
  LogIn,
  Plus,
  RefreshCw,
  SplitSquareHorizontal,
  SplitSquareVertical,
  TerminalSquare,
  UserPlus,
  X,
} from "lucide-react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import "./styles.css";
import { Button } from "./components/ui/button";
import { Input } from "./components/ui/input";
import { Select } from "./components/ui/select";
import { Card } from "./components/ui/card";
import { cn, controlDeviceId, makeStreamId, wsBaseFromLocation } from "./lib/utils";

type WorkerView = {
  id: string;
  tenant_id?: string;
  name: string;
  addr: string;
  last_seen: string;
};

type SessionView = {
  id: string;
  tenant_id?: string;
  worker_id: string;
  name: string;
  cwd: string;
  command: string;
  status: string;
};

type SplitDirection = "horizontal" | "vertical";
type DropZone = "left" | "right" | "top" | "bottom" | "center";

type PaneNode = {
  type: "pane";
  id: string;
  sessionId?: string;
};

type SplitNode = {
  type: "split";
  id: string;
  direction: SplitDirection;
  children: LayoutNode[];
};

type LayoutNode = PaneNode | SplitNode;

type Status = {
  tone: "idle" | "ok" | "warn" | "err";
  title: string;
  detail: string;
};

type AuthMode = "login" | "register";

type AuthCredentialPayload = {
  credential: string;
  credential_id: string;
  tenant_id: string;
  device_id: string;
  user?: {
    email: string;
    name: string;
  };
};

type DragPayload =
  | {
      kind: "session";
      sessionId: string;
    }
  | {
      kind: "pane";
      paneId: string;
    };

type DropTarget = {
  paneId: string;
  zone: DropZone;
};

const dragMime = "application/x-agentmux-drag";
const query = new URLSearchParams(window.location.search);
const initialSignal = query.get("signal") || "";
const initialToken = query.get("token") || localStorage.getItem("agentmux.token") || "";
const initialPaneId = crypto.randomUUID();

function App() {
  const [token, setToken] = React.useState(initialToken);
  const [workers, setWorkers] = React.useState<WorkerView[]>([]);
  const [sessions, setSessions] = React.useState<SessionView[]>([]);
  const [sidebarOpen, setSidebarOpen] = React.useState(true);
  const [authMode, setAuthMode] = React.useState<AuthMode>("login");
  const [authForm, setAuthForm] = React.useState({ email: "", password: "", name: "" });
  const [layout, setLayout] = React.useState<LayoutNode>({ type: "pane", id: initialPaneId });
  const [activePane, setActivePane] = React.useState<string>(initialPaneId);
  const [dropTarget, setDropTarget] = React.useState<DropTarget | null>(null);
  const [status, setStatus] = React.useState<Status>({
    tone: "idle",
    title: "No session attached",
    detail: "Open a session from the sidebar.",
  });
  const [createForm, setCreateForm] = React.useState({
    worker_id: "",
    name: "demo",
    cwd: ".",
    command: "bash",
  });

  React.useEffect(() => {
    if (initialSignal) {
      void exchangeSignal(initialSignal);
    } else if (initialToken) {
      void refreshAll(initialToken);
    }
  }, []);

  React.useEffect(() => {
    if (!createForm.worker_id && workers[0]) {
      setCreateForm((form) => ({ ...form, worker_id: workers[0].id }));
    }
  }, [workers, createForm.worker_id]);

  async function exchangeSignal(signal: string) {
    setStatus({ tone: "warn", title: "Exchanging signal", detail: "Requesting a browser credential." });
    const res = await fetch("/api/exchange", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        signal,
        role: "control",
        device_id: controlDeviceId(),
        device_name: browserDeviceName(),
      }),
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Signal exchange failed", detail: await res.text() });
      return;
    }
    const data = await res.json();
    await acceptCredential(data, "Signal credential ready");
  }

  async function apiFetch(path: string, init: RequestInit = {}, authToken = token) {
    const headers = new Headers(init.headers);
    if (authToken) headers.set("Authorization", `Bearer ${authToken}`);
    if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    return fetch(path, { ...init, headers });
  }

  async function refreshAll(authToken = token) {
    localStorage.setItem("agentmux.token", authToken);
    const [workersRes, sessionsRes] = await Promise.all([apiFetch("/api/workers", {}, authToken), apiFetch("/api/sessions", {}, authToken)]);
    if (!workersRes.ok || !sessionsRes.ok) {
      setStatus({ tone: "err", title: "Unauthorized or hub unavailable", detail: `${workersRes.status} ${sessionsRes.status}` });
      return;
    }
    const workersPayload = await workersRes.json();
    const sessionsPayload = await sessionsRes.json();
    setWorkers(workersPayload.workers || []);
    setSessions(sessionsPayload.sessions || []);
    setStatus({ tone: "ok", title: "Hub synced", detail: `${workersPayload.workers?.length || 0} workers · ${sessionsPayload.sessions?.length || 0} sessions` });
  }

  async function createSession(event: React.FormEvent) {
    event.preventDefault();
    const res = await apiFetch("/api/sessions", { method: "POST", body: JSON.stringify(createForm) });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Create failed", detail: await res.text() });
      return;
    }
    setStatus({ tone: "warn", title: "Create queued", detail: `${createForm.worker_id}/${createForm.name}` });
    window.setTimeout(() => void refreshAll(), 700);
  }

  async function submitAuth(event: React.FormEvent) {
    event.preventDefault();
    const path = authMode === "register" ? "/api/auth/register" : "/api/auth/login";
    const res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        email: authForm.email,
        password: authForm.password,
        name: authForm.name,
        device_id: controlDeviceId(),
        device_name: browserDeviceName(),
      }),
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: authMode === "register" ? "Registration failed" : "Login failed", detail: await res.text() });
      return;
    }
    await acceptCredential(await res.json(), authMode === "register" ? "Account created" : "Signed in");
  }

  async function acceptCredential(data: AuthCredentialPayload, title: string) {
    setToken(data.credential);
    localStorage.setItem("agentmux.token", data.credential);
    localStorage.setItem("agentmux.control_device_id", data.device_id);
    setStatus({
      tone: "ok",
      title,
      detail: `${data.user?.email || data.credential_id} · ${data.tenant_id}`,
    });
    await refreshAll(data.credential);
  }

  async function startOAuth(provider: "github" | "google") {
    const res = await fetch(`/api/auth/oauth/${provider}?device_id=${encodeURIComponent(controlDeviceId())}`);
    const text = await res.text();
    if (!res.ok) {
      setStatus({ tone: "warn", title: `${provider} OAuth not configured`, detail: errorDetail(text) });
      return;
    }
    const data = JSON.parse(text);
    if (data.url) window.location.href = data.url;
  }

  function attach(sessionId: string) {
    setLayout((node) => updatePane(node, activePane, (pane) => ({ ...pane, sessionId })));
  }

  function splitPane(direction: SplitDirection) {
    splitPaneById(activePane, direction);
  }

  function splitPaneById(paneId: string, direction: SplitDirection) {
    const pane = newPane();
    setLayout((node) => splitPaneNode(node, paneId, direction, pane).node);
    setActivePane(pane.id);
  }

  function dropOnPane(targetPaneId: string, zone: DropZone, payload: DragPayload) {
    setDropTarget(null);
    if (payload.kind === "session") {
      const pane = zone === "center" ? undefined : newPane(payload.sessionId);
      setLayout((node) => {
        if (zone === "center") return updatePane(node, targetPaneId, (pane) => ({ ...pane, sessionId: payload.sessionId }));
        return insertPaneRelative(node, targetPaneId, zone, pane!).node;
      });
      setActivePane(pane?.id || targetPaneId);
      return;
    }
    if (payload.paneId === targetPaneId) return;
    if (zone === "center") {
      setLayout((node) => swapPaneSessions(node, payload.paneId, targetPaneId));
      setActivePane(targetPaneId);
      return;
    }
    setLayout((node) => {
      const extracted = extractPane(node, payload.paneId);
      if (!extracted.pane || !extracted.node || !findPane(extracted.node, targetPaneId)) return node;
      const inserted = insertPaneRelative(extracted.node, targetPaneId, zone, extracted.pane);
      return inserted.inserted ? inserted.node : node;
    });
    setActivePane(payload.paneId);
  }

  function closePane(id: string) {
    setLayout((node) => {
      const result = removePane(node, id);
      const next = result.node || newPane();
      if (id === activePane || !findPane(next, activePane)) {
        setActivePane(firstPaneId(next));
      }
      return next;
    });
  }

  const activeSessionIds = new Set(collectPanes(layout).map((pane) => pane.sessionId).filter(Boolean));

  return (
    <div className="flex h-screen bg-background text-foreground">
      <aside className={cn("flex h-full shrink-0 flex-col border-r border-border bg-card transition-all", sidebarOpen ? "w-80" : "w-0 overflow-hidden")}>
        <div className="flex h-14 items-center justify-between border-b border-border px-3">
          <div className="flex items-center gap-2 text-sm font-semibold">
            <TerminalSquare className="h-4 w-4 text-primary" />
            AgentMux
          </div>
          <Button variant="ghost" size="icon" onClick={() => setSidebarOpen(false)} title="Hide sidebar">
            <ChevronLeft className="h-4 w-4" />
          </Button>
        </div>
        <div className="space-y-3 border-b border-border p-3">
          <div className="grid grid-cols-2 rounded-md bg-secondary p-1">
            <Button
              variant={authMode === "login" ? "default" : "ghost"}
              size="sm"
              onClick={() => setAuthMode("login")}
              type="button"
            >
              <LogIn className="h-4 w-4" />
              Sign in
            </Button>
            <Button
              variant={authMode === "register" ? "default" : "ghost"}
              size="sm"
              onClick={() => setAuthMode("register")}
              type="button"
            >
              <UserPlus className="h-4 w-4" />
              Register
            </Button>
          </div>
          <form className="space-y-2" onSubmit={submitAuth}>
            <Input type="email" value={authForm.email} onChange={(event) => setAuthForm({ ...authForm, email: event.target.value })} placeholder="email" autoComplete="email" />
            {authMode === "register" ? (
              <Input value={authForm.name} onChange={(event) => setAuthForm({ ...authForm, name: event.target.value })} placeholder="display name" autoComplete="name" />
            ) : null}
            <Input type="password" value={authForm.password} onChange={(event) => setAuthForm({ ...authForm, password: event.target.value })} placeholder="password" autoComplete={authMode === "register" ? "new-password" : "current-password"} />
            <Button variant="secondary" className="w-full" type="submit">
              {authMode === "register" ? <UserPlus className="h-4 w-4" /> : <LogIn className="h-4 w-4" />}
              {authMode === "register" ? "Create Account" : "Sign In"}
            </Button>
          </form>
          <div className="grid grid-cols-2 gap-2">
            <Button variant="ghost" size="sm" type="button" onClick={() => void startOAuth("github")}>
              <Github className="h-4 w-4" />
              GitHub
            </Button>
            <Button
              variant="ghost"
              size="sm"
              type="button"
              onClick={() => void startOAuth("google")}
            >
              <Globe className="h-4 w-4" />
              Google
            </Button>
          </div>
          <Input value={token} onChange={(event) => setToken(event.target.value)} placeholder="amx_cred_... or dev token" spellCheck={false} />
          <Button variant="secondary" className="w-full" onClick={() => void refreshAll()}>
            <RefreshCw className="h-4 w-4" />
            Refresh
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto p-2">
          <div className="mb-2 px-1 text-xs font-medium uppercase text-muted-foreground">Sessions</div>
          <div className="space-y-1">
            {sessions.length === 0 ? <div className="px-2 py-4 text-sm text-muted-foreground">No sessions.</div> : null}
            {sessions.map((session) => (
              <button
                key={session.id}
                draggable
                className={cn(
                  "w-full rounded-md border border-transparent px-2 py-2 text-left hover:border-border hover:bg-secondary",
                  activeSessionIds.has(session.id) && "border-primary/40 bg-primary/10",
                )}
                onClick={() => attach(session.id)}
                onDragStart={(event) => setDragPayload(event, { kind: "session", sessionId: session.id })}
                onDragEnd={() => setDropTarget(null)}
              >
                <div className="truncate text-sm font-medium">{session.id}</div>
                <div className="truncate text-xs text-muted-foreground">{session.command || "shell"} · {session.status || "unknown"}</div>
                <div className="truncate text-xs text-muted-foreground">{session.cwd}</div>
              </button>
            ))}
          </div>
        </div>
        <form className="space-y-2 border-t border-border p-3" onSubmit={createSession}>
          <div className="text-xs font-medium uppercase text-muted-foreground">Create</div>
          <Select value={createForm.worker_id} onChange={(event) => setCreateForm({ ...createForm, worker_id: event.target.value })}>
            <option value="">Select worker</option>
            {workers.map((worker) => (
              <option key={worker.id} value={worker.id}>{worker.id}</option>
            ))}
          </Select>
          <Input value={createForm.name} onChange={(event) => setCreateForm({ ...createForm, name: event.target.value })} placeholder="session name" />
          <Input value={createForm.cwd} onChange={(event) => setCreateForm({ ...createForm, cwd: event.target.value })} placeholder="working directory" />
          <Input value={createForm.command} onChange={(event) => setCreateForm({ ...createForm, command: event.target.value })} placeholder="command" />
          <Button className="w-full" type="submit">
            <Plus className="h-4 w-4" />
            Create Session
          </Button>
        </form>
      </aside>

      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center justify-between border-b border-border bg-card px-3">
          <div className="flex min-w-0 items-center gap-2">
            {!sidebarOpen ? (
              <Button variant="ghost" size="icon" onClick={() => setSidebarOpen(true)} title="Show sidebar">
                <ChevronRight className="h-4 w-4" />
              </Button>
            ) : null}
            <LayoutGrid className="h-4 w-4 text-muted-foreground" />
            <div className="min-w-0">
              <div className="truncate text-sm font-medium">{status.title}</div>
              <div className="truncate text-xs text-muted-foreground">{status.detail}</div>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <span className={cn("h-2 w-2 rounded-full", status.tone === "ok" && "bg-emerald-500", status.tone === "warn" && "bg-amber-500", status.tone === "err" && "bg-red-500", status.tone === "idle" && "bg-muted-foreground")} />
            <Button variant="secondary" size="sm" onClick={() => splitPane("horizontal")} title="Split right">
              <SplitSquareHorizontal className="h-4 w-4" />
              Right
            </Button>
            <Button variant="secondary" size="sm" onClick={() => splitPane("vertical")} title="Split down">
              <SplitSquareVertical className="h-4 w-4" />
              Down
            </Button>
          </div>
        </header>
        <div className="min-h-0 flex-1">
          <LayoutRenderer
            node={layout}
            activePane={activePane}
            token={token}
            onFocusPane={setActivePane}
            onSplitPane={splitPaneById}
            onClosePane={closePane}
            dropTarget={dropTarget}
            onDropTarget={setDropTarget}
            onDropPayload={dropOnPane}
            setStatus={setStatus}
          />
        </div>
      </main>
    </div>
  );
}

function LayoutRenderer({
  node,
  activePane,
  token,
  onFocusPane,
  onSplitPane,
  onClosePane,
  dropTarget,
  onDropTarget,
  onDropPayload,
  setStatus,
}: {
  node: LayoutNode;
  activePane: string;
  token: string;
  onFocusPane: (id: string) => void;
  onSplitPane: (paneId: string, direction: SplitDirection) => void;
  onClosePane: (id: string) => void;
  dropTarget: DropTarget | null;
  onDropTarget: (target: DropTarget | null) => void;
  onDropPayload: (paneId: string, zone: DropZone, payload: DragPayload) => void;
  setStatus: React.Dispatch<React.SetStateAction<Status>>;
}) {
  if (node.type === "pane") {
    return (
      <TerminalPane
        pane={node}
        active={node.id === activePane}
        token={token}
        onFocus={() => onFocusPane(node.id)}
        onSplit={(direction) => onSplitPane(node.id, direction)}
        onClose={() => onClosePane(node.id)}
        dropTarget={dropTarget?.paneId === node.id ? dropTarget : null}
        onDropTarget={onDropTarget}
        onDropPayload={(zone, payload) => onDropPayload(node.id, zone, payload)}
        setStatus={setStatus}
      />
    );
  }
  return (
    <PanelGroup direction={node.direction} className="h-full w-full min-h-0">
      {node.children.map((child, index) => (
        <React.Fragment key={child.id}>
          {index > 0 ? (
            <PanelResizeHandle
              className={cn(
                "bg-border transition-colors hover:bg-primary",
                node.direction === "horizontal" ? "w-1" : "h-1",
              )}
            />
          ) : null}
          <Panel minSize={15} className="min-h-0 min-w-0 overflow-hidden">
            <LayoutRenderer
              node={child}
              activePane={activePane}
              token={token}
              onFocusPane={onFocusPane}
              onSplitPane={onSplitPane}
              onClosePane={onClosePane}
              dropTarget={dropTarget}
              onDropTarget={onDropTarget}
              onDropPayload={onDropPayload}
              setStatus={setStatus}
            />
          </Panel>
        </React.Fragment>
      ))}
    </PanelGroup>
  );
}

function TerminalPane({
  pane,
  active,
  token,
  onFocus,
  onSplit,
  onClose,
  dropTarget,
  onDropTarget,
  onDropPayload,
  setStatus,
}: {
  pane: PaneNode;
  active: boolean;
  token: string;
  onFocus: () => void;
  onSplit: (direction: SplitDirection) => void;
  onClose: () => void;
  dropTarget: DropTarget | null;
  onDropTarget: (target: DropTarget | null) => void;
  onDropPayload: (zone: DropZone, payload: DragPayload) => void;
  setStatus: React.Dispatch<React.SetStateAction<Status>>;
}) {
  const terminalRef = React.useRef<HTMLDivElement | null>(null);
  const terminal = React.useRef<Terminal | null>(null);
  const fit = React.useRef<FitAddon | null>(null);
  const socket = React.useRef<WebSocket | null>(null);
  const streamId = React.useRef("");
  const lastSize = React.useRef({ cols: 0, rows: 0 });

  React.useEffect(() => {
    if (!pane.sessionId || !terminalRef.current) return;
    const term = new Terminal({
      cursorBlink: true,
      scrollback: 5000,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 14,
      theme: { background: "#050607", foreground: "#eef2f3", cursor: "#35c98f" },
    });
    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    terminalRef.current.innerHTML = "";
    term.open(terminalRef.current);
    terminal.current = term;
    fit.current = fitAddon;
    term.focus();

    streamId.current = makeStreamId(pane.sessionId);
    const ws = new WebSocket(`${wsBaseFromLocation()}/ws/control?token=${encodeURIComponent(token)}`);
    socket.current = ws;

    const fitTerminal = () => {
      if (!terminalRef.current) return;
      const rect = terminalRef.current.getBoundingClientRect();
      if (rect.width < 20 || rect.height < 20) return;
      fitAddon.fit();
      return { cols: term.cols, rows: term.rows };
    };
    const fitAndSendResize = () => {
      const size = fitTerminal();
      if (!size) return;
      if (term.cols === lastSize.current.cols && term.rows === lastSize.current.rows) return;
      lastSize.current = size;
      if (ws.readyState === WebSocket.OPEN) {
        sendEnvelope(ws, "terminal.resize", pane.sessionId!, streamId.current, size);
      }
    };
    const scheduleFit = () => {
      requestAnimationFrame(() => {
        fitAndSendResize();
        requestAnimationFrame(fitAndSendResize);
      });
    };
    scheduleFit();

    ws.addEventListener("open", () => {
      const size = fitTerminal();
      sendEnvelope(ws, "control.open", pane.sessionId!, streamId.current, size || { cols: term.cols, rows: term.rows });
      if (size) lastSize.current = size;
      setStatus({ tone: "ok", title: `Attached ${pane.sessionId}`, detail: streamId.current });
      scheduleFit();
    });
    ws.addEventListener("message", (event) => {
      const env = JSON.parse(event.data);
      if (env.type === "terminal.output") {
        const payload = env.payload || {};
        if (payload.encoding === "base64") term.write(base64ToBytes(payload.data || ""));
        else term.write(payload.data || "");
      }
      if (env.type === "error") {
        setStatus({ tone: "err", title: "Remote error", detail: env.payload?.message || "unknown error" });
      }
    });
    ws.addEventListener("close", () => setStatus({ tone: "warn", title: `Detached ${pane.sessionId}`, detail: "The browser stream closed." }));
    term.onData((data) => sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data }));
    const keyHandler = (event: KeyboardEvent) => {
      if (!terminalRef.current || !terminalRef.current.contains(event.target as Node)) return;
      if (!shouldManuallyCaptureTerminalKey(event)) return;
      const data = encodeKeyEvent(event);
      if (!data) return;
      event.preventDefault();
      event.stopPropagation();
      event.stopImmediatePropagation();
      sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data });
    };
    document.addEventListener("keydown", keyHandler, true);

    const observer = new ResizeObserver(() => {
      scheduleFit();
    });
    observer.observe(terminalRef.current);

    return () => {
      document.removeEventListener("keydown", keyHandler, true);
      observer.disconnect();
      if (ws.readyState === WebSocket.OPEN) sendEnvelope(ws, "terminal.close", pane.sessionId!, streamId.current, {});
      ws.close();
      term.dispose();
      terminal.current = null;
      fit.current = null;
      socket.current = null;
    };
  }, [pane.sessionId, token, setStatus]);

  return (
    <div
      className={cn("relative flex h-full min-h-0 min-w-0 flex-col overflow-hidden border-l border-transparent bg-[#050607]", active && "border-l-primary")}
      onMouseDown={onFocus}
      onDragOver={(event) => {
        const payload = readDragPayload(event);
        if (!payload) return;
        event.preventDefault();
        event.dataTransfer.dropEffect = payload.kind === "pane" ? "move" : "copy";
        onDropTarget({ paneId: pane.id, zone: dropZoneFromEvent(event) });
      }}
      onDragLeave={(event) => {
        if (event.currentTarget.contains(event.relatedTarget as Node | null)) return;
        onDropTarget(null);
      }}
      onDrop={(event) => {
        const payload = readDragPayload(event);
        if (!payload) return;
        event.preventDefault();
        onDropPayload(dropZoneFromEvent(event), payload);
      }}
    >
      <div className="flex h-9 items-center justify-between border-b border-border bg-card px-2">
        <div
          className="min-w-0 flex-1 cursor-grab truncate text-xs font-medium text-muted-foreground active:cursor-grabbing"
          draggable
          onDragStart={(event) => setDragPayload(event, { kind: "pane", paneId: pane.id })}
          onDragEnd={() => onDropTarget(null)}
          title="Drag pane"
        >
          {pane.sessionId || "Empty pane"}
        </div>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon"
            onClick={(event) => {
              event.stopPropagation();
              onSplit("horizontal");
            }}
            title="Split right"
          >
            <SplitSquareHorizontal className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            onClick={(event) => {
              event.stopPropagation();
              onSplit("vertical");
            }}
            title="Split down"
          >
            <SplitSquareVertical className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            onClick={(event) => {
              event.stopPropagation();
              onClose();
            }}
            title="Close pane"
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      </div>
      <div className="min-h-0 min-w-0 flex-1 overflow-hidden p-2">
        {pane.sessionId ? (
          <div ref={terminalRef} className="h-full w-full min-w-0 overflow-hidden" />
        ) : (
          <Card className="grid h-full place-items-center border-dashed bg-background">
            <div className="text-center text-sm text-muted-foreground">Select a session from the sidebar.</div>
          </Card>
        )}
      </div>
      <DropIndicator zone={dropTarget?.zone} />
    </div>
  );
}

function DropIndicator({ zone }: { zone?: DropZone }) {
  if (!zone) return null;
  return (
    <div className="pointer-events-none absolute inset-0 z-10">
      <div
        className={cn(
          "absolute rounded-sm border border-primary bg-primary/20",
          zone === "center" && "inset-3",
          zone === "left" && "inset-y-2 left-2 w-1/3",
          zone === "right" && "inset-y-2 right-2 w-1/3",
          zone === "top" && "inset-x-2 top-2 h-1/3",
          zone === "bottom" && "inset-x-2 bottom-2 h-1/3",
        )}
      />
    </div>
  );
}

function newPane(sessionId?: string): PaneNode {
  return { type: "pane", id: crypto.randomUUID(), sessionId };
}

function updatePane(node: LayoutNode, paneId: string, update: (pane: PaneNode) => PaneNode): LayoutNode {
  if (node.type === "pane") {
    return node.id === paneId ? update(node) : node;
  }
  return { ...node, children: node.children.map((child) => updatePane(child, paneId, update)) };
}

function splitPaneNode(node: LayoutNode, paneId: string, direction: SplitDirection, pane: PaneNode): { node: LayoutNode; found: boolean } {
  if (node.type === "pane") {
    if (node.id !== paneId) return { node, found: false };
    return { node: { type: "split", id: crypto.randomUUID(), direction, children: [node, pane] }, found: true };
  }
  let found = false;
  const children = node.children.map((child) => {
    if (found) return child;
    const result = splitPaneNode(child, paneId, direction, pane);
    found = result.found;
    return result.node;
  });
  return { node: { ...node, children }, found };
}

function insertPaneRelative(node: LayoutNode, paneId: string, zone: DropZone, pane: PaneNode): { node: LayoutNode; inserted: boolean } {
  if (zone === "center") {
    return { node: updatePane(node, paneId, () => pane), inserted: true };
  }
  const direction = zone === "left" || zone === "right" ? "horizontal" : "vertical";
  if (node.type === "pane") {
    if (node.id !== paneId) return { node, inserted: false };
    const children = zone === "left" || zone === "top" ? [pane, node] : [node, pane];
    return { node: { type: "split", id: crypto.randomUUID(), direction, children }, inserted: true };
  }
  let inserted = false;
  const children = node.children.map((child) => {
    if (inserted) return child;
    const result = insertPaneRelative(child, paneId, zone, pane);
    inserted = result.inserted;
    return result.node;
  });
  return { node: { ...node, children }, inserted };
}

function removePane(node: LayoutNode, paneId: string): { node?: LayoutNode; removed: boolean } {
  if (node.type === "pane") {
    return node.id === paneId ? { removed: true } : { node, removed: false };
  }
  let removed = false;
  const children = node.children.flatMap((child) => {
    const result = removePane(child, paneId);
    if (result.removed) removed = true;
    return result.node ? [result.node] : [];
  });
  if (!removed) return { node, removed: false };
  if (children.length === 0) return { removed: true };
  if (children.length === 1) return { node: children[0], removed: true };
  return { node: { ...node, children }, removed: true };
}

function extractPane(node: LayoutNode, paneId: string): { node?: LayoutNode; pane?: PaneNode; removed: boolean } {
  if (node.type === "pane") {
    return node.id === paneId ? { pane: node, removed: true } : { node, removed: false };
  }
  let removed = false;
  let pane: PaneNode | undefined;
  const children = node.children.flatMap((child) => {
    const result = extractPane(child, paneId);
    if (result.removed) {
      removed = true;
      pane = result.pane;
    }
    return result.node ? [result.node] : [];
  });
  if (!removed) return { node, removed: false };
  if (children.length === 0) return { pane, removed: true };
  if (children.length === 1) return { node: children[0], pane, removed: true };
  return { node: { ...node, children }, pane, removed: true };
}

function swapPaneSessions(node: LayoutNode, sourcePaneId: string, targetPaneId: string): LayoutNode {
  const source = findPane(node, sourcePaneId);
  const target = findPane(node, targetPaneId);
  if (!source || !target) return node;
  return updatePane(
    updatePane(node, sourcePaneId, (pane) => ({ ...pane, sessionId: target.sessionId })),
    targetPaneId,
    (pane) => ({ ...pane, sessionId: source.sessionId }),
  );
}

function collectPanes(node: LayoutNode): PaneNode[] {
  if (node.type === "pane") return [node];
  return node.children.flatMap(collectPanes);
}

function findPane(node: LayoutNode, paneId: string): PaneNode | undefined {
  if (node.type === "pane") return node.id === paneId ? node : undefined;
  for (const child of node.children) {
    const pane = findPane(child, paneId);
    if (pane) return pane;
  }
  return undefined;
}

function firstPaneId(node: LayoutNode): string {
  return node.type === "pane" ? node.id : firstPaneId(node.children[0]);
}

function setDragPayload(event: React.DragEvent, payload: DragPayload) {
  event.dataTransfer.effectAllowed = payload.kind === "pane" ? "move" : "copyMove";
  event.dataTransfer.setData(dragMime, JSON.stringify(payload));
  event.dataTransfer.setData("text/plain", payload.kind === "session" ? payload.sessionId : payload.paneId);
}

function readDragPayload(event: React.DragEvent): DragPayload | null {
  const value = event.dataTransfer.getData(dragMime);
  if (!value) return null;
  try {
    const payload = JSON.parse(value) as DragPayload;
    if (payload.kind === "session" && payload.sessionId) return payload;
    if (payload.kind === "pane" && payload.paneId) return payload;
  } catch {
    return null;
  }
  return null;
}

function dropZoneFromEvent(event: React.DragEvent<HTMLElement>): DropZone {
  const rect = event.currentTarget.getBoundingClientRect();
  const x = (event.clientX - rect.left) / Math.max(rect.width, 1);
  const y = (event.clientY - rect.top) / Math.max(rect.height, 1);
  const edge = 0.28;
  if (x < edge) return "left";
  if (x > 1 - edge) return "right";
  if (y < edge) return "top";
  if (y > 1 - edge) return "bottom";
  return "center";
}

function sendEnvelope(ws: WebSocket, type: string, sessionId: string, streamId: string, payload: unknown) {
  if (ws.readyState !== WebSocket.OPEN) return;
  ws.send(JSON.stringify({ type, session_id: sessionId, stream_id: streamId, payload }));
}

function base64ToBytes(value: string) {
  const text = atob(value);
  const bytes = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i++) bytes[i] = text.charCodeAt(i);
  return bytes;
}

function shouldManuallyCaptureTerminalKey(event: KeyboardEvent) {
  if (event.isComposing || event.metaKey) return false;
  return event.ctrlKey || event.altKey || event.key === "Tab";
}

function encodeKeyEvent(event: KeyboardEvent) {
  const key = event.key;
  if (key === "Enter") return "\r";
  if (key === "Tab") return event.shiftKey ? "\x1b[Z" : "\t";
  if (key === "Backspace") return "\x7f";
  if (key === "Escape") return "\x1b";
  if (key === "Delete") return "\x1b[3~";
  if (key === "Insert") return "\x1b[2~";
  if (key === "Home") return "\x1b[H";
  if (key === "End") return "\x1b[F";
  if (key === "PageUp") return "\x1b[5~";
  if (key === "PageDown") return "\x1b[6~";
  if (key === "ArrowUp") return "\x1b[A";
  if (key === "ArrowDown") return "\x1b[B";
  if (key === "ArrowRight") return "\x1b[C";
  if (key === "ArrowLeft") return "\x1b[D";
  if (/^F([1-9]|1[0-2])$/.test(key)) return functionKeySequence(key);
  if (event.ctrlKey && !event.altKey) return controlSequence(key);
  if (event.altKey && key.length === 1) return "\x1b" + key;
  if (!event.ctrlKey && !event.altKey && key.length === 1) return key;
  return "";
}

function controlSequence(key: string) {
  if (key === " ") return "\x00";
  if (key === "[") return "\x1b";
  if (key === "\\") return "\x1c";
  if (key === "]") return "\x1d";
  if (key === "^") return "\x1e";
  if (key === "_" || key === "-") return "\x1f";
  if (key.length === 1) {
    const code = key.toUpperCase().charCodeAt(0);
    if (code >= 65 && code <= 90) return String.fromCharCode(code - 64);
  }
  return "";
}

function functionKeySequence(key: string) {
  return (
    {
      F1: "\x1bOP",
      F2: "\x1bOQ",
      F3: "\x1bOR",
      F4: "\x1bOS",
      F5: "\x1b[15~",
      F6: "\x1b[17~",
      F7: "\x1b[18~",
      F8: "\x1b[19~",
      F9: "\x1b[20~",
      F10: "\x1b[21~",
      F11: "\x1b[23~",
      F12: "\x1b[24~",
    } as Record<string, string>
  )[key] || "";
}

function browserDeviceName() {
  return `web:${navigator.platform || "browser"}`;
}

function errorDetail(text: string) {
  try {
    const payload = JSON.parse(text);
    return payload.error || text;
  } catch {
    return text;
  }
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
