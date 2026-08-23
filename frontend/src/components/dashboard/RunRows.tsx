import { Monitor } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { formatRelative, formatTime } from "../../lib/api";
import { providerLabel } from "../../lib/providers";
import type { Run } from "../../types";
import { Badge } from "../ui/Badge";

export function RunRowHeader({ dated = false }: { dated?: boolean }) {
  return <div className="run-row run-row-header"><span /><span>Status</span><span>Name</span><span>Source</span><span className="url-column">URL</span><span>{dated ? "Time" : "Started"}</span></div>;
}

export function RunRow({ run, dated = false }: { run: Run; dated?: boolean }) {
  const navigate = useNavigate();
  return <button className="run-row" onClick={() => navigate(`/runs/${run.id}`)}>
    <span className="run-thumbnail"><Monitor size={15} /></span>
    <span><Badge status={run.status} /></span>
    <span className="run-name"><strong>{run.instructions}</strong></span>
    <span><em className="source-badge">{providerLabel(run.provider)}</em></span>
    <span className="run-url url-column">{run.url.replace(/^https?:\/\//, "")}</span>
    <time>{dated ? formatTime(run.created_at) : formatRelative(run.created_at)}</time>
  </button>;
}
