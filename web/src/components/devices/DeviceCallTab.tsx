import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CallRegular, DeleteRegular, MicRegular, Speaker2Regular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button } from "../ui";
import { Input } from "../ui/Input";

interface CellularCall {
  index?: number;
  direction?: number;
  state?: number;
  number?: string;
  raw?: string;
}

interface CallsResponse {
  transport?: string;
  calls?: CellularCall[];
  raw?: string;
}

type Phase = "idle" | "dialing" | "active" | "ended" | "failed";

interface MediaState {
  status: "closed" | "connecting" | "open" | "error";
  detail: string;
}

const KEYPAD = ["1", "2", "3", "4", "5", "6", "7", "8", "9", "*", "0", "#"];

const CLCC_ACTIVE = 0;
const CLCC_HELD = 1;
const CLCC_DIALING = 2;
const CLCC_ALERTING = 3;

function digitsOf(value: string): string {
  return (value || "").replace(/\D/g, "");
}

// Downsample the microphone's Float32 stream to 8 kHz signed 16-bit PCM. The
// modem's USB audio codec expects exactly the telephony rate in both
// directions.
function downsampleToPCM(input: Float32Array, sourceRate: number): Int16Array {
  const ratio = sourceRate / 8000;
  const length = Math.floor(input.length / ratio);
  const output = new Int16Array(length);
  for (let index = 0; index < length; index += 1) {
    const start = Math.floor(index * ratio);
    const end = Math.min(input.length, Math.floor((index + 1) * ratio));
    let sum = 0;
    for (let cursor = start; cursor < end; cursor += 1) sum += input[cursor];
    const value = end > start ? sum / (end - start) : 0;
    output[index] = Math.max(-32768, Math.min(32767, Math.round(value * 32767)));
  }
  return output;
}

export function DeviceCallTab({
  deviceId,
  deviceOnline,
  active,
}: {
  deviceId: string;
  deviceOnline: boolean;
  active: boolean;
}) {
  const { t } = useI18n();
  const [number, setNumber] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [transport, setTransport] = useState("");
  const [calls, setCalls] = useState<CellularCall[]>([]);
  const [elapsed, setElapsed] = useState(0);
  const [error, setError] = useState("");
  const [media, setMedia] = useState<MediaState>({ status: "closed", detail: "" });
  const [micAllowed, setMicAllowed] = useState<boolean | null>(null);

  const dialedRef = useRef("");
  const startRef = useRef<number>(0);
  const wasActiveRef = useRef(false);
  const wsRef = useRef<WebSocket | null>(null);
  const audioCtxRef = useRef<AudioContext | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const processorRef = useRef<ScriptProcessorNode | null>(null);
  const sourceRef = useRef<MediaStreamAudioSourceNode | null>(null);
  const nextPlayTimeRef = useRef(0);
  const phaseRef = useRef<Phase>("idle");

  const secureContext = typeof window === "undefined" ? true : window.isSecureContext;

  const stopAudio = useCallback(() => {
    const ws = wsRef.current;
    wsRef.current = null;
    if (ws) {
      try {
        ws.close();
      } catch {
        // ignore
      }
    }
    streamRef.current?.getTracks().forEach((track) => track.stop());
    streamRef.current = null;
    processorRef.current?.disconnect();
    processorRef.current = null;
    sourceRef.current?.disconnect();
    sourceRef.current = null;
    const ctx = audioCtxRef.current;
    audioCtxRef.current = null;
    if (ctx && ctx.state !== "closed") {
      void ctx.close();
    }
    nextPlayTimeRef.current = 0;
    setMedia({ status: "closed", detail: "" });
  }, []);

  const startAudio = useCallback(
    async (callId: string) => {
      stopAudio();
      if (!navigator.mediaDevices?.getUserMedia) {
        setMedia({ status: "error", detail: t("当前浏览器不支持麦克风采集") });
        setMicAllowed(false);
        return;
      }
      setMedia({ status: "connecting", detail: t("正在申请麦克风并建立音频通道…") });
      let stream: MediaStream;
      try {
        stream = await navigator.mediaDevices.getUserMedia({
          audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        });
      } catch (cause) {
        setMicAllowed(false);
        setMedia({
          status: "error",
          detail:
            cause instanceof Error && cause.name === "NotAllowedError"
              ? t("麦克风权限被拒绝，请在浏览器地址栏允许麦克风后重试")
              : t("无法打开麦克风：需要 HTTPS 或 localhost 访问本页面"),
        });
        return;
      }
      const AudioContextCtor = window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
      if (!AudioContextCtor) {
        stream.getTracks().forEach((track) => track.stop());
        setMedia({ status: "error", detail: t("当前浏览器不支持 Web Audio") });
        return;
      }
      streamRef.current = stream;
      setMicAllowed(true);
      const context = new AudioContextCtor();
      audioCtxRef.current = context;
      await context.resume().catch(() => undefined);
      const socket = new WebSocket(
        `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/devices/${encodeURIComponent(deviceId)}/calls/media?call_id=${encodeURIComponent(callId)}`,
      );
      socket.binaryType = "arraybuffer";
      wsRef.current = socket;
      socket.onopen = () => {
        setMedia({ status: "open", detail: t("音频通道已连接") });
      };
      socket.onerror = () => {
        setMedia({ status: "error", detail: t("音频通道连接失败") });
      };
      socket.onclose = (event) => {
        if (wsRef.current === socket) {
          wsRef.current = null;
          setMedia((previous) =>
            previous.status === "open"
              ? { status: "closed", detail: t("音频通道已断开") }
              : previous.status === "error"
                ? previous
                : { status: "error", detail: event.reason || t("音频通道已关闭") },
          );
        }
      };
      socket.onmessage = (event) => {
        if (!(event.data instanceof ArrayBuffer) || !audioCtxRef.current) return;
        const samples = new Int16Array(event.data);
        if (samples.length === 0) return;
        const ctx = audioCtxRef.current;
        const buffer = ctx.createBuffer(1, samples.length, 8000);
        const channel = buffer.getChannelData(0);
        for (let index = 0; index < samples.length; index += 1) {
          channel[index] = samples[index] / 32768;
        }
        const player = ctx.createBufferSource();
        player.buffer = buffer;
        player.connect(ctx.destination);
        const startAt = Math.max(ctx.currentTime + 0.06, nextPlayTimeRef.current);
        player.start(startAt);
        nextPlayTimeRef.current = startAt + buffer.duration;
      };
      const source = context.createMediaStreamSource(stream);
      sourceRef.current = source;
      const processor = context.createScriptProcessor(1024, 1, 1);
      processorRef.current = processor;
      processor.onaudioprocess = (event) => {
        const socket = wsRef.current;
        if (!socket || socket.readyState !== WebSocket.OPEN) return;
        const pcm = downsampleToPCM(event.inputBuffer.getChannelData(0), context.sampleRate);
        if (pcm.length > 0) socket.send(pcm.buffer);
      };
      const silent = context.createGain();
      silent.gain.value = 0;
      source.connect(processor);
      processor.connect(silent);
      silent.connect(context.destination);
    },
    [deviceId, stopAudio, t],
  );

  const refreshCalls = useCallback(async () => {
    try {
      const data = await api<CallsResponse>(`/devices/${deviceId}/calls`);
      setTransport(data?.transport || "");
      const list = Array.isArray(data?.calls) ? data.calls : [];
      setCalls(list);
      const dialed = digitsOf(dialedRef.current);
      const match = list.find((call) => {
        const digits = digitsOf(call.number || "");
        return digits.length > 0 && (digits === dialed || digits.endsWith(dialed) || dialed.endsWith(digits));
      });
      if (match) {
        const state = match.state ?? -1;
        if (state === CLCC_ACTIVE || state === CLCC_HELD) {
          wasActiveRef.current = true;
          setPhase("active");
        } else if (state === CLCC_DIALING || state === CLCC_ALERTING) {
          setPhase((previous) => (previous === "active" ? previous : "dialing"));
        }
      } else if (wasActiveRef.current) {
        wasActiveRef.current = false;
        setPhase("ended");
        stopAudio();
      }
    } catch {
      // Transient polling failures must not tear down an in-progress call.
    }
  }, [deviceId, stopAudio]);

  useEffect(() => {
    if (!active && phase === "idle") return undefined;
    void refreshCalls();
    const timer = window.setInterval(() => void refreshCalls(), 1500);
    return () => window.clearInterval(timer);
  }, [active, phase, refreshCalls]);

  useEffect(() => {
    phaseRef.current = phase;
  }, [phase]);

  // Selecting another device must not leak audio or a stale call state.
  useEffect(() => {
    stopAudio();
    setPhase("idle");
    setElapsed(0);
    setCalls([]);
    setError("");
    dialedRef.current = "";
    startRef.current = 0;
    wasActiveRef.current = false;
    return () => {
      stopAudio();
      if (phaseRef.current === "dialing" || phaseRef.current === "active") {
        // Leaving the page ends the call instead of leaving it billed in the
        // background; the tab switch itself keeps the component mounted.
        void api(`/devices/${deviceId}/calls/hangup`, { method: "POST", body: { call_id: "" } }).catch(() => undefined);
      }
    };
  }, [deviceId, stopAudio]);

  useEffect(() => {
    if (phase !== "active" && phase !== "dialing") return undefined;
    const timer = window.setInterval(() => {
      if (startRef.current > 0) setElapsed(Math.floor((Date.now() - startRef.current) / 1000));
    }, 1000);
    return () => window.clearInterval(timer);
  }, [phase]);

  useEffect(() => () => stopAudio(), [stopAudio]);

  const append = (digit: string) => {
    setError("");
    setNumber((previous) => (previous.length >= 32 ? previous : previous + digit));
  };

  const dial = async () => {
    const target = number.trim();
    if (!target) {
      setError(t("请输入要拨打的号码"));
      return;
    }
    setError("");
    setPhase("dialing");
    setElapsed(0);
    wasActiveRef.current = false;
    dialedRef.current = target;
    startRef.current = Date.now();
    try {
      const result = await api<{ call_id?: string }>(`/devices/${deviceId}/calls/dial`, {
        method: "POST",
        body: { number: target, duration_seconds: 0 },
      });
      const callId = String(result?.call_id || "");
      await startAudio(callId);
    } catch (cause) {
      setPhase("failed");
      startRef.current = 0;
      setError(apiMessage(cause) || t("拨号失败"));
    }
  };

  const hangup = async () => {
    try {
      await api(`/devices/${deviceId}/calls/hangup`, {
        method: "POST",
        body: { call_id: "" },
      });
    } catch (cause) {
      setError(apiMessage(cause) || t("挂断失败"));
    } finally {
      stopAudio();
      startRef.current = 0;
      wasActiveRef.current = false;
      setPhase("idle");
      setElapsed(0);
    }
  };

  const busy = phase === "dialing" || phase === "active";
  const statusText = useMemo(() => {
    switch (phase) {
      case "dialing":
        return t("正在拨号…");
      case "active":
        return t("通话中");
      case "ended":
        return t("通话已结束");
      case "failed":
        return t("拨号失败");
      default:
        return deviceOnline ? t("空闲") : t("设备离线");
    }
  }, [phase, deviceOnline, t]);

  const mmss = `${String(Math.floor(elapsed / 60)).padStart(2, "0")}:${String(elapsed % 60).padStart(2, "0")}`;

  return (
    <div className="space-y-4">
      <div className="rounded-2xl border border-gray-200 bg-white/70 p-5 dark:border-gray-700 dark:bg-gray-900/40">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-sm font-semibold text-gray-800 dark:text-gray-100">{t("网络拨号（LTE 语音）")}</div>
            <div className="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {t("通过蜂窝基站建立通话，音频经模组 USB 音频接口与浏览器双向传输。")}
            </div>
          </div>
          <div className="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
            <span className={`inline-block h-2 w-2 rounded-full ${busy ? "bg-emerald-500" : "bg-gray-400"}`} />
            <span>{statusText}</span>
            {busy ? <span className="font-mono">{mmss}</span> : null}
            {transport ? <span className="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">{transport === "vowifi" ? "VoWiFi" : "LTE"}</span> : null}
          </div>
        </div>

        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_260px]">
          <div className="space-y-4">
            <Input
              value={number}
              onChange={(event) => setNumber(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !busy) void dial();
              }}
              placeholder={t("输入电话号码，例如 10000")}
              disabled={busy}
              className="font-mono text-lg tracking-wider"
            />
            <div className="grid grid-cols-3 gap-2">
              {KEYPAD.map((digit) => (
                <button
                  key={digit}
                  type="button"
                  disabled={busy}
                  onClick={() => append(digit)}
                  className="rounded-xl border border-gray-200 bg-white py-3 text-lg font-medium text-gray-700 transition hover:border-indigo-300 hover:text-indigo-600 disabled:opacity-40 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-200"
                >
                  {digit}
                </button>
              ))}
            </div>
          </div>

          <div className="flex flex-col gap-2">
            {busy ? (
              <Button variant="danger" className="flex-1" icon={<CallRegular />} onClick={() => void hangup()}>
                {t("挂断")}
              </Button>
            ) : (
              <Button
                variant="primary"
                className="flex-1"
                icon={<CallRegular />}
                disabled={!deviceOnline}
                onClick={() => void dial()}
              >
                {t("拨打")}
              </Button>
            )}
            <Button
              icon={<DeleteRegular />}
              disabled={busy || !number}
              onClick={() => setNumber((previous) => previous.slice(0, -1))}
            >
              {t("退格")}
            </Button>
            <Button disabled={busy || !number} onClick={() => setNumber("")}>
              {t("清空")}
            </Button>
          </div>
        </div>

        {error ? (
          <div className="mt-4 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-600 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
            {error}
          </div>
        ) : null}

        <div className="mt-4 grid gap-2 text-xs text-gray-500 dark:text-gray-400 sm:grid-cols-2">
          <div className="flex items-center gap-2">
            <MicRegular className={media.status === "open" ? "text-emerald-500" : ""} />
            <span>
              {t("上行")}: {media.status === "open" ? t("已连接") : media.status === "connecting" ? t("连接中") : t("未连接")}
            </span>
          </div>
          <div className="flex items-center gap-2">
            <Speaker2Regular className={media.status === "open" ? "text-emerald-500" : ""} />
            <span>
              {t("下行")}: {media.detail || t("等待通话建立")}
            </span>
          </div>
        </div>

        {media.status === "error" ? (
          <div className="mt-2 text-xs text-amber-600 dark:text-amber-400">{media.detail}</div>
        ) : null}
        {!secureContext ? (
          <div className="mt-2 text-xs text-amber-600 dark:text-amber-400">
            {t("浏览器只在 HTTPS 或 localhost 下允许麦克风；当前为 HTTP 访问，可能只能听到对方声音。")}
          </div>
        ) : null}
        {micAllowed === false && media.status !== "error" ? (
          <div className="mt-2 text-xs text-amber-600 dark:text-amber-400">{t("麦克风不可用，通话可能只有单向声音。")}</div>
        ) : null}
      </div>

      {calls.length > 0 ? (
        <div className="rounded-2xl border border-gray-200 bg-white/70 p-4 dark:border-gray-700 dark:bg-gray-900/40">
          <div className="mb-2 text-xs font-bold uppercase tracking-wider text-gray-500">{t("模组通话列表")}</div>
          <div className="space-y-1 font-mono text-xs text-gray-600 dark:text-gray-300">
            {calls.map((call, index) => (
              <div key={`${call.index ?? index}-${call.raw || index}`} className="truncate">
                {call.raw || JSON.stringify(call)}
              </div>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}
