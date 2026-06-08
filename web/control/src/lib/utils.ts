import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function wsBaseFromLocation() {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}`;
}

export function makeStreamId(sessionId: string) {
  const id = crypto.randomUUID ? crypto.randomUUID() : Math.random().toString(36).slice(2);
  return `web-${Date.now()}-${id}|${sessionId}|`;
}

export function controlDeviceId() {
  const key = "agentmux.control_device_id";
  const existing = localStorage.getItem(key);
  if (existing) return existing;
  const id = `web_${crypto.randomUUID ? crypto.randomUUID() : Math.random().toString(36).slice(2)}`;
  localStorage.setItem(key, id);
  return id;
}
