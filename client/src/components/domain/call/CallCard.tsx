import { useEffect, useRef, useState } from "react";
import {
  Check,
  Mic,
  MicOff,
  PhoneOff,
  RotateCcw,
  RotateCw,
  Video,
  VideoOff,
  WifiOff,
} from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { StatusBadge } from "@/components/ui/status-badge";
import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { attachMeter } from "@/lib/audio-meter";
import { cvoRotationToCss } from "@/lib/call/video-codec";
import { useCalls } from "@/stores/calls";
import { useDevices } from "@/stores/devices";
import { useEndCall } from "@/hooks/useEndCall";
import { useSetMute } from "@/hooks/useSetMute";
import { useResumeCall } from "@/hooks/useResumeCall";
import { needsAudioResume } from "@/lib/resume";
import { useT } from "@/hooks/useT";
import { callStatusTone, callStatusPulse } from "@/lib/status";
import { PeerAvatar } from "@/components/domain/contacts/PeerAvatar";
import { waveLevel } from "@/lib/waveform";
import { formatCallDuration } from "@/utils/format";
import type {
  CallStatus,
  CallSummary,
  QualitySample,
  SetupMark,
} from "@/types/call";

const waveBars = [
  { h: 8, delay: "0s" },
  { h: 14, delay: "0.12s" },
  { h: 20, delay: "0.24s" },
  { h: 13, delay: "0.12s" },
  { h: 9, delay: "0s" },
];

const Waveform = ({ db }: { db: number }) => (
  <div
    className="flex h-5 items-center gap-[3px]"
    style={{ transform: `scaleY(${waveLevel(db)})` }}
  >
    {waveBars.map((bar, i) => (
      <span
        key={i}
        className="tom-wave w-[2.5px] rounded-full bg-primary"
        style={{ height: `${bar.h}px`, animationDelay: bar.delay }}
      />
    ))}
  </div>
);

const Meter = ({ label, db }: { label: string; db: number }) => (
  <div className="space-y-1">
    <p className="text-xs text-muted-foreground">{label}</p>
    <Waveform db={db} />
  </div>
);

type QualityTone = "ok" | "warn" | "bad" | "idle";

const toneFill: Record<QualityTone, string> = {
  ok: "bg-primary",
  warn: "bg-amber-500",
  bad: "bg-destructive",
  idle: "bg-muted-foreground/40",
};

const toneText: Record<QualityTone, string> = {
  ok: "text-primary",
  warn: "text-amber-600 dark:text-amber-400",
  bad: "text-destructive",
  idle: "text-muted-foreground",
};

const clampPct = (value: number, full: number) =>
  Math.max(0, Math.min(100, (value / full) * 100));

const rttTone = (ms: number): QualityTone =>
  ms <= 150 ? "ok" : ms <= 300 ? "warn" : "bad";
const jitterTone = (ms: number): QualityTone =>
  ms <= 30 ? "ok" : ms <= 50 ? "warn" : "bad";
const lossTone = (frac: number): QualityTone =>
  frac <= 0.01 ? "ok" : frac <= 0.05 ? "warn" : "bad";

const QualityBar = ({
  label,
  value,
  pct,
  tone,
}: {
  label: string;
  value: string;
  pct: number;
  tone: QualityTone;
}) => (
  <div className="space-y-1">
    <div className="flex items-baseline justify-between">
      <span className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span className={`font-mono text-xs font-semibold ${toneText[tone]}`}>
        {value}
      </span>
    </div>
    <div className="h-2 overflow-hidden rounded-full bg-muted">
      <div
        className={`h-full transition-all ${toneFill[tone]}`}
        style={{ width: `${pct}%` }}
      />
    </div>
  </div>
);

const QualityPanel = ({ q }: { q: QualitySample | undefined }) => {
  const t = useT();
  if (!q) {
    return (
      <p className="text-xs text-muted-foreground">
        {t.calls.measuringQuality}
      </p>
    );
  }
  const lossPct = q.lossFraction * 100;
  return (
    <div className="space-y-2 rounded-md border border-border/60 p-2">
      {q.hasRtt ? (
        <QualityBar
          label={t.calls.rtt}
          value={`${Math.round(q.rttMs)} ms`}
          pct={clampPct(q.rttMs, 400)}
          tone={rttTone(q.rttMs)}
        />
      ) : (
        <QualityBar label={t.calls.rtt} value="—" pct={0} tone="idle" />
      )}
      <QualityBar
        label={t.calls.jitter}
        value={`${Math.round(q.jitterMs)} ms`}
        pct={clampPct(q.jitterMs, 80)}
        tone={jitterTone(q.jitterMs)}
      />
      <QualityBar
        label={t.calls.loss}
        value={`${lossPct.toFixed(1)}%`}
        pct={clampPct(lossPct, 10)}
        tone={lossTone(q.lossFraction)}
      />
    </div>
  );
};

const markSteps = [
  { key: "transport.ice", label: "ICE" },
  { key: "transport.dtls", label: "DTLS" },
  { key: "transport.sctp_open", label: "SCTP" },
  { key: "transport.stun", label: "STUN" },
  { key: "media.first_packet", label: "Media" },
];

const ConnectionTimeline = ({
  marks,
  status,
}: {
  marks: SetupMark[];
  status: CallStatus;
}) => {
  const [expanded, setExpanded] = useState(false);
  const t = useT();
  const byMark = new Map(marks.map((m) => [m.mark, m.elapsedMs]));
  const mediaMs = byMark.get("media.first_packet");

  if (status === "connected" && mediaMs !== undefined && !expanded) {
    return (
      <button
        type="button"
        onClick={() => setExpanded(true)}
        className="flex w-full items-center gap-2 rounded-md border border-border/60 px-2 py-1.5 text-xs text-muted-foreground"
      >
        <Check className="h-3.5 w-3.5 text-primary" />
        {t.calls.connectedIn} <span className="font-mono">{mediaMs} ms</span>
      </button>
    );
  }

  return (
    <div className="space-y-2 rounded-md border border-border/60 p-2">
      <div className="flex items-center justify-between">
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          {t.calls.connection}
        </span>
        {mediaMs !== undefined && (
          <button
            type="button"
            onClick={() => setExpanded(false)}
            className="font-mono text-xs text-muted-foreground"
          >
            {mediaMs} ms
          </button>
        )}
      </div>
      <div className="grid grid-cols-5 gap-1">
        {markSteps.map((step) => {
          const ms = byMark.get(step.key);
          const reached = ms !== undefined;
          return (
            <div key={step.key} className="flex flex-col items-center gap-1">
              <div
                className={`h-2.5 w-2.5 rounded-full ${reached ? "bg-primary" : "bg-muted-foreground/40"}`}
              />
              <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
                {step.label}
              </span>
              <span
                className={`font-mono text-[10px] ${reached ? "text-foreground" : "text-muted-foreground"}`}
              >
                {reached ? `${ms}` : "—"}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
};

const ReconnectingNotice = () => {
  const t = useT();
  const [showHint, setShowHint] = useState(false);
  return (
    <div className="space-y-1.5 rounded-md border border-amber-500/30 bg-amber-500/10 px-2 py-1.5 text-xs text-amber-600 dark:text-amber-400">
      <div className="flex items-center gap-2">
        <WifiOff className="h-3.5 w-3.5" />
        {t.calls.reconnectingMedia}
      </div>
      <button
        type="button"
        onClick={() => setShowHint((v) => !v)}
        className="underline underline-offset-2"
      >
        {t.calls.reconnectWhy}
      </button>
      {showHint && <p className="leading-relaxed">{t.calls.reconnectHint}</p>}
    </div>
  );
};

export const CallCard = ({ call }: { call: CallSummary }) => {
  const conn = useCalls((s) => s.ownConnections.get(call.callId));
  const quality = useCalls((s) => s.quality.get(call.callId));
  const marks = useCalls((s) => s.marks.get(call.callId));
  const peerMuted = useCalls((s) => s.peerMuted.get(call.callId) ?? false);
  const outDeviceId = useDevices((s) => s.outId);
  const micId = useDevices((s) => s.micId);
  const endCall = useEndCall();
  const setMute = useSetMute();
  const resume = useResumeCall(call.sessionId, micId);
  const t = useT();
  // A CallCard only renders for calls this browser owns (CallsPage filters by isMine),
  // so a detached (no local connection) established call is one whose bridge a refresh
  // dropped here. Another browser's call never reaches this card.
  const detached = needsAudioResume(call.status, !!conn);
  const autoResumed = useRef(false);
  const [, force] = useState(0);
  const [micDb, setMicDb] = useState(-60);
  const [peerDb, setPeerDb] = useState(-60);
  // Mute lives on the mic track (enabled=false keeps zeroed frames flowing so the
  // peer's media watchdog stays fed); re-derive it on mount so a card remount
  // during a muted call does not desync the button.
  const [muted, setMutedState] = useState(
    () => conn?.micStream.getAudioTracks().some((tr) => !tr.enabled) ?? false,
  );
  const audioRef = useRef<HTMLAudioElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const localVideoRef = useRef<HTMLVideoElement>(null);
  const [hasVideo, setHasVideo] = useState(false);
  const [rotation, setRotation] = useState(0);
  // WhatsApp encodes the phone's camera as landscape and signals no reliable rotation for the
  // browser (it lives in the proprietary WASM), so we default portrait phones (landscape pixels)
  // to a 90 deg turn and let the operator adjust the direction by hand.
  const [peerRotate, setPeerRotate] = useState(0);
  const rotatedManually = useRef(false);
  const hasLocalCamera = !!conn?.localVideoStream;
  const [cameraOn, setCameraOn] = useState(true);
  // Show the immersive video stage as soon as this is a video call: either the peer's video has
  // arrived or we are sending our own camera. Waiting for the remote track alone would hide the
  // self-view and the camera/mic controls whenever the peer's downlink has not locked yet.
  const immersive = (hasVideo || hasLocalCamera) && !detached;

  const toggleCamera = () => {
    if (!conn) return;
    setCameraOn(conn.toggleCamera());
  };

  const toggleMute = () => {
    if (!conn) return;
    const next = !muted;
    conn.micStream.getAudioTracks().forEach((tr) => (tr.enabled = !next));
    setMutedState(next);
    setMute.mutate(
      { sid: call.sessionId, callId: call.callId, muted: next },
      {
        onError: () => {
          conn.micStream.getAudioTracks().forEach((tr) => (tr.enabled = next));
          setMutedState(!next);
        },
      },
    );
  };

  useEffect(() => {
    const timer = setInterval(() => force((n) => n + 1), 1000);
    return () => clearInterval(timer);
  }, []);

  // Auto-resume the audio once when the call comes back detached (e.g. after a
  // refresh). A single attempt avoids re-prompting for the mic in a loop; a failure
  // surfaces the manual "Reconnect call" button below.
  useEffect(() => {
    if (detached && !autoResumed.current && !resume.isPending) {
      autoResumed.current = true;
      resume.mutate({ callId: call.callId, video: call.video });
    }
    if (!detached) autoResumed.current = false;
  }, [detached, resume, call.callId, call.video]);

  useEffect(() => {
    if (!conn) return;
    const offMic = attachMeter(conn.micStream, setMicDb);
    let offPeer: (() => void) | null = null;
    const wait = setInterval(() => {
      if (conn.remoteStream && audioRef.current) {
        audioRef.current.srcObject = conn.remoteStream;
        audioRef.current.play().catch(() => {});
        offPeer = attachMeter(conn.remoteStream, setPeerDb);
        clearInterval(wait);
      }
    }, 200);
    return () => {
      offMic();
      offPeer?.();
      clearInterval(wait);
    };
  }, [conn]);

  useEffect(() => {
    const el = audioRef.current as
      (HTMLAudioElement & { setSinkId?: (id: string) => Promise<void> }) | null;
    if (!el || !outDeviceId || typeof el.setSinkId !== "function") return;
    el.setSinkId(outDeviceId).catch(() => {});
  }, [outDeviceId, conn]);

  useEffect(() => {
    const stream = conn?.remoteVideoStream;
    if (!stream) return;
    if (videoRef.current) videoRef.current.srcObject = stream;
    const sync = () => setHasVideo(stream.getVideoTracks().length > 0);
    stream.addEventListener("addtrack", sync);
    stream.addEventListener("removetrack", sync);
    conn.onRotation(setRotation);
    const raf = requestAnimationFrame(sync);
    return () => {
      cancelAnimationFrame(raf);
      stream.removeEventListener("addtrack", sync);
      stream.removeEventListener("removetrack", sync);
    };
  }, [conn]);

  useEffect(() => {
    if (localVideoRef.current) {
      localVideoRef.current.srcObject = conn?.localVideoStream ?? null;
    }
  }, [conn, immersive]);

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-start justify-between gap-3">
          <div className="flex min-w-0 items-center gap-3">
            <PeerAvatar
              name={call.peerName || call.peer}
              photoUrl={call.peerPhotoUrl}
            />
            <div className="min-w-0">
              <p className="truncate font-medium">
                {call.peerName || call.peer}
              </p>
              <StatusBadge
                tone={callStatusTone(call.status)}
                pulse={callStatusPulse(call.status)}
                className="mt-1"
              >
                {call.status === "connected"
                  ? formatCallDuration(call.startedAt)
                  : t.calls.status[call.status]}
              </StatusBadge>
            </div>
          </div>
          {!immersive && (
            <div className="flex items-center gap-2">
              {call.status === "connected" && conn && (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Button
                      variant={muted ? "secondary" : "outline"}
                      size="icon"
                      onClick={toggleMute}
                      aria-label={muted ? t.calls.unmute : t.calls.mute}
                      aria-pressed={muted}
                    >
                      {muted ? (
                        <MicOff className="h-4 w-4" />
                      ) : (
                        <Mic className="h-4 w-4" />
                      )}
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent>
                    {muted ? t.calls.unmute : t.calls.mute}
                  </TooltipContent>
                </Tooltip>
              )}
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="destructive"
                    size="icon"
                    onClick={() =>
                      endCall.mutate({
                        sid: call.sessionId,
                        callId: call.callId,
                      })
                    }
                    aria-label={t.calls.endCall}
                  >
                    <PhoneOff className="h-4 w-4" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t.calls.endCall}</TooltipContent>
              </Tooltip>
            </div>
          )}
        </div>
        <div
          className={
            immersive
              ? "relative overflow-hidden rounded-xl border border-border/60 bg-black"
              : "hidden"
          }
        >
          <video
            ref={videoRef}
            autoPlay
            playsInline
            muted
            onLoadedMetadata={(e) => {
              const v = e.currentTarget;
              // Phone front-camera video arrives landscape and upright-relative rotation is 270deg
              // (a -90deg turn); +90 would land upside down. Operator can still adjust by hand.
              if (!rotatedManually.current && v.videoWidth > v.videoHeight) {
                setPeerRotate(270);
              }
            }}
            className="mx-auto block max-h-[60vh] w-full object-contain"
            style={{
              transform: `rotate(${(cvoRotationToCss(rotation) + peerRotate) % 360}deg)`,
            }}
          />
          {hasLocalCamera && (
            <video
              ref={localVideoRef}
              autoPlay
              playsInline
              muted
              className="absolute bottom-16 right-3 aspect-video w-24 rounded-lg border border-white/25 object-cover shadow-lg sm:bottom-20 sm:w-36"
              style={{ transform: "scaleX(-1)" }}
            />
          )}
          {peerMuted && (
            <div className="absolute left-3 top-3 flex items-center gap-1.5 rounded-full bg-black/55 px-2.5 py-1 text-xs text-white/90 backdrop-blur">
              <MicOff className="h-3.5 w-3.5" />
              {t.calls.peerMuted}
            </div>
          )}
          <div className="absolute inset-x-0 bottom-0 flex items-center justify-center gap-3 bg-gradient-to-t from-black/70 to-transparent p-3">
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="secondary"
                  size="icon"
                  className="h-11 w-11 rounded-full border-none bg-white/15 text-white hover:bg-white/25"
                  onClick={() => {
                    rotatedManually.current = true;
                    setPeerRotate((r) => (r + 90) % 360);
                  }}
                  aria-label={t.calls.rotateVideo}
                >
                  <RotateCw className="h-5 w-5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t.calls.rotateVideo}</TooltipContent>
            </Tooltip>
            {hasLocalCamera && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="secondary"
                    size="icon"
                    className="h-11 w-11 rounded-full border-none bg-white/15 text-white hover:bg-white/25"
                    onClick={toggleCamera}
                    aria-label={cameraOn ? t.calls.cameraOff : t.calls.cameraOn}
                    aria-pressed={!cameraOn}
                  >
                    {cameraOn ? (
                      <Video className="h-5 w-5" />
                    ) : (
                      <VideoOff className="h-5 w-5" />
                    )}
                  </Button>
                </TooltipTrigger>
                <TooltipContent>
                  {cameraOn ? t.calls.cameraOff : t.calls.cameraOn}
                </TooltipContent>
              </Tooltip>
            )}
            {conn && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="secondary"
                    size="icon"
                    className="h-11 w-11 rounded-full border-none bg-white/15 text-white hover:bg-white/25"
                    onClick={toggleMute}
                    aria-label={muted ? t.calls.unmute : t.calls.mute}
                    aria-pressed={muted}
                  >
                    {muted ? (
                      <MicOff className="h-5 w-5" />
                    ) : (
                      <Mic className="h-5 w-5" />
                    )}
                  </Button>
                </TooltipTrigger>
                <TooltipContent>
                  {muted ? t.calls.unmute : t.calls.mute}
                </TooltipContent>
              </Tooltip>
            )}
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="destructive"
                  size="icon"
                  className="h-11 w-11 rounded-full"
                  onClick={() =>
                    endCall.mutate({ sid: call.sessionId, callId: call.callId })
                  }
                  aria-label={t.calls.endCall}
                >
                  <PhoneOff className="h-5 w-5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t.calls.endCall}</TooltipContent>
            </Tooltip>
          </div>
        </div>
        {call.status === "reconnecting" && <ReconnectingNotice />}
        {marks && marks.length > 0 && (
          <ConnectionTimeline marks={marks} status={call.status} />
        )}
        {detached ? (
          <div className="flex items-center justify-between gap-3 rounded-md border border-border/60 px-3 py-2 text-xs text-muted-foreground">
            <span className="flex items-center gap-1.5">
              <RotateCcw
                className={`h-3.5 w-3.5 ${resume.isPending ? "animate-spin" : ""}`}
              />
              {resume.isPending
                ? t.calls.reconnectingAudio
                : t.calls.reconnectDropped}
            </span>
            {!resume.isPending && (
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  resume.mutate({ callId: call.callId, video: call.video })
                }
              >
                {t.calls.reconnectCall}
              </Button>
            )}
          </div>
        ) : (
          !immersive && (
            <>
              <Meter label={t.calls.mic} db={micDb} />
              <Meter label={t.calls.peer} db={peerDb} />
            </>
          )
        )}
        {peerMuted && !immersive && (
          <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <MicOff className="h-3.5 w-3.5" />
            {t.calls.peerMuted}
          </div>
        )}
        {call.status === "connected" && <QualityPanel q={quality} />}
        <audio ref={audioRef} autoPlay />
      </CardContent>
    </Card>
  );
};
