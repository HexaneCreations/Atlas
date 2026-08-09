import {
  LayoutDashboard,
  Server,
  Container,
  Cpu,
  Layers,
  Clock,
  Network,
  HardDrive,
  type LucideIcon,
} from "lucide-react";

/**
 * The single source of truth for Atlas's pages: the sidebar's grouping and
 * the command palette's flat list are both views over this one list, so
 * adding a page can never update one without the other.
 */
export interface NavPage {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  section: "Monitor" | "Infrastructure";
}

export const NAV_PAGES: NavPage[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true, section: "Monitor" },
  { to: "/nodes", label: "Nodes", icon: Server, section: "Monitor" },
  { to: "/containers", label: "Containers", icon: Container, section: "Monitor" },
  { to: "/processes", label: "Processes", icon: Cpu, section: "Monitor" },
  { to: "/services", label: "Services", icon: Layers, section: "Monitor" },
  { to: "/cron", label: "Scheduled jobs", icon: Clock, section: "Infrastructure" },
  { to: "/ports", label: "Ports", icon: Network, section: "Infrastructure" },
  { to: "/disks", label: "Disks", icon: HardDrive, section: "Infrastructure" },
];
