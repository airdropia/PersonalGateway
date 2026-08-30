// Sidebar navigation registry: the ordered list of dashboard pages with
// their lucide icons and optional runtime-config visibility gates.
// Each item is { page, label, icon, visible?, notify? }: `page` is the route
// id under /admin/dashboard/{page} (see $lib/stores/router), `label` is a
// generated Paraglide message function, `icon` is a lucide icon
// imported below and passed to the Icon atom, `visible` a feature gate, and
// `notify` marks the item with a dot when it has something waiting. Both
// read runes stores, so call them inside a reactive context (Sidebar's
// $derived and markup) to re-evaluate when the underlying state changes.

import { versionStore } from "$lib/stores/version.svelte.js";
import * as m from "$lib/paraglide/messages.js";
import {
  Box,
  ChartColumn,
  FlaskConical,
  KeyRound,
  LayoutDashboard,
  ServerCog,
  Settings,
} from "lucide";

export const NAV_ITEMS = [
  { page: "overview", label: m.navigation_overview, icon: LayoutDashboard },
  { page: "providers-config", label: m.navigation_providers, icon: ServerCog },
  { page: "models", label: m.navigation_models, icon: Box },
  { page: "playground", label: m.navigation_playground, icon: FlaskConical },
  { page: "usage", label: m.navigation_usage, icon: ChartColumn },
  { page: "auth-keys", label: m.navigation_api_keys, icon: KeyRound },
  {
    page: "settings",
    label: m.navigation_settings,
    icon: Settings,
    // A newer release is announced on the Settings page; the dot is what
    // makes an operator look there.
    notify: () => versionStore.updateAvailable,
  },
];