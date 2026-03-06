import { NavLink, Outlet } from "react-router";
import { useAuth } from "@/lib/auth";
import { useSSE, type SSEStatus } from "@/lib/sse";
import { useTheme } from "@/lib/theme";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { AkashiBrand, AkashiLogo } from "@/components/AkashiLogo";
import {
  LayoutDashboard,
  FileText,
  Users,
  AlertTriangle,
  Search,
  Download,
  Shield,
  LogOut,
  Menu,
  Moon,
  Sun,
  X,
} from "lucide-react";
import { useState } from "react";
import { cn } from "@/lib/utils";

function ConnectionDot({ status }: { status: SSEStatus }) {
  const colors: Record<SSEStatus, string> = {
    connected: "bg-emerald-500 shadow-[0_0_8px_2px] shadow-emerald-500/50 animate-pulse-glow text-emerald-500",
    connecting: "bg-amber-500 animate-pulse",
    disconnected: "bg-red-500",
  };
  const labels: Record<SSEStatus, string> = {
    connected: "Live",
    connecting: "Connecting",
    disconnected: "Offline",
  };
  return (
    <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
      <span className={cn("h-2 w-2 rounded-full", colors[status])} />
      {labels[status]}
    </span>
  );
}

const navItems = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard },
  { to: "/decisions", label: "Decisions", icon: FileText },
  { to: "/agents", label: "Agents", icon: Users },
  { to: "/conflicts", label: "Conflicts", icon: AlertTriangle },
  { to: "/search", label: "Search", icon: Search },
  { to: "/export", label: "Export", icon: Download },
  { to: "/grants", label: "Grants", icon: Shield },
];

export default function Layout() {
  const { agentId, token, logout } = useAuth();
  const sseStatus = useSSE(token);
  const { theme, toggle: toggleTheme } = useTheme();
  const [sidebarOpen, setSidebarOpen] = useState(false);

  return (
    <div className="flex h-screen overflow-hidden">
      {/* Mobile overlay */}
      {sidebarOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/60 backdrop-blur-sm lg:hidden"
          onClick={() => setSidebarOpen(false)}
          aria-hidden="true"
        />
      )}

      {/* Sidebar */}
      <aside
        className={cn(
          "fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r bg-[hsl(var(--sidebar))] transition-transform lg:static lg:translate-x-0",
          sidebarOpen ? "translate-x-0" : "-translate-x-full",
        )}
      >
        {/* Brand header */}
        <div className="flex h-14 items-center justify-between border-b px-4">
          <AkashiBrand />
          <button
            className="lg:hidden text-muted-foreground hover:text-foreground transition-colors"
            onClick={() => setSidebarOpen(false)}
            aria-label="Close sidebar"
          >
            <X className="h-5 w-5" />
          </button>
        </div>

        {/* Navigation */}
        <nav className="flex-1 space-y-1 p-3" aria-label="Main navigation">
          {navItems.map(({ to, label, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              end={to === "/"}
              onClick={() => setSidebarOpen(false)}
              className={({ isActive }) =>
                cn(
                  "group relative flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-all duration-200",
                  isActive
                    ? "bg-primary/10 text-primary shadow-[inset_0_0_20px_-8px_hsl(var(--glow-blue)/0.15)]"
                    : "text-muted-foreground hover:bg-accent hover:text-accent-foreground hover:translate-x-0.5",
                )
              }
            >
              {({ isActive }) => (
                <>
                  {isActive && (
                    <span className="absolute left-0 inset-y-1 w-[3px] rounded-r-full bg-gradient-to-b from-primary via-primary/80 to-purple-500/60" />
                  )}
                  <Icon className={cn("h-4 w-4 transition-all duration-200", isActive && "drop-shadow-[0_0_4px_hsl(var(--glow-blue)/0.5)]")} />
                  {label}
                </>
              )}
            </NavLink>
          ))}
        </nav>

        {/* Footer */}
        <div className="border-t border-t-border/50 p-4 space-y-3">
          <div className="flex items-center justify-between">
            <ConnectionDot status={sseStatus} />
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 text-muted-foreground hover:text-foreground"
              onClick={toggleTheme}
              aria-label={theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
            >
              {theme === "dark" ? <Sun className="h-3.5 w-3.5" /> : <Moon className="h-3.5 w-3.5" />}
            </Button>
          </div>
          <div className="flex items-center justify-between">
            <Badge variant="outline" className="text-xs font-mono">
              {agentId}
            </Badge>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 text-muted-foreground hover:text-foreground"
              onClick={logout}
              aria-label="Logout"
            >
              <LogOut className="h-3.5 w-3.5" />
            </Button>
          </div>
        </div>
      </aside>

      {/* Main content */}
      <div className="flex flex-1 flex-col overflow-hidden bg-atmosphere">
        <header className="flex h-14 items-center gap-4 border-b px-4 lg:hidden">
          <button onClick={() => setSidebarOpen(true)} aria-label="Open sidebar">
            <Menu className="h-5 w-5" />
          </button>
          <AkashiLogo className="h-6 w-6" />
          <span className="text-lg font-bold">Akashi</span>
        </header>
        <main className="flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
