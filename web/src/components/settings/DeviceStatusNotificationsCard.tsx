import { useEffect, useState } from "react";
import { AlertRegular } from "@fluentui/react-icons";
import { api, apiMessage } from "../../api";
import { useI18n } from "../../lib/i18n";
import { Button, message } from "../ui";
import { Input } from "../ui/Input";
import { Switch } from "../ui/Switch";
import { CardDecor, CardIcon, CardTitle } from "./Cards";
import { Field } from "./controls";

interface DeviceStatusNotificationSettings {
  enabled: boolean;
  device_offline: boolean;
  network_lost: boolean;
  offline_grace_seconds: number;
  cooldown_seconds: number;
}

const DEFAULTS: DeviceStatusNotificationSettings = {
  enabled: false,
  device_offline: true,
  network_lost: true,
  offline_grace_seconds: 60,
  cooldown_seconds: 600,
};

// Device status alerts reuse whatever push channels are already enabled; this
// card only controls the detection thresholds and which transitions alert.
export function DeviceStatusNotificationsCard() {
  const { t } = useI18n();
  const [value, setValue] = useState<DeviceStatusNotificationSettings>(DEFAULTS);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const data = await api<DeviceStatusNotificationSettings>("/settings/device-notifications");
        if (!cancelled && data) {
          setValue({ ...DEFAULTS, ...data });
        }
      } catch {
        if (!cancelled) message.error(t("设备通知配置加载失败"));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const update = (patch: Partial<DeviceStatusNotificationSettings>) => setValue((prev) => ({ ...prev, ...patch }));

  const save = async () => {
    setSaving(true);
    try {
      const data = await api<DeviceStatusNotificationSettings>("/settings/device-notifications", {
        method: "PUT",
        body: {
          ...value,
          offline_grace_seconds: Number(value.offline_grace_seconds) || 60,
          cooldown_seconds: Number(value.cooldown_seconds) || 600,
        },
      });
      if (data) setValue({ ...DEFAULTS, ...data });
      message.success(t("设备通知配置已保存"));
    } catch (error) {
      message.error(apiMessage(error) || t("设备通知配置保存失败"));
    } finally {
      setSaving(false);
    }
  };

  const off = !value.enabled || loading;
  return (
    <div className="ui-card group relative overflow-hidden p-8">
      <CardDecor />
      <div className="relative z-10 mb-4 flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <CardIcon>
            <AlertRegular className="text-[24px]" />
          </CardIcon>
          <CardTitle
            title={t("设备掉线与掉网通知")}
            subtitle={t("设备断开或蜂窝网络丢失时，通过已启用的推送渠道提醒")}
          />
        </div>
        <Switch
          checked={value.enabled}
          disabled={loading || saving}
          loading={saving}
          onChange={(enabled) => update({ enabled })}
        />
      </div>
      <div className="relative z-10 space-y-4">
        <div className="flex items-center justify-between gap-4">
          <span className="text-sm text-gray-600 dark:text-gray-300">{t("设备掉线（USB 断开/硬件离线）")}</span>
          <Switch checked={value.device_offline} disabled={off} onChange={(device_offline) => update({ device_offline })} />
        </div>
        <div className="flex items-center justify-between gap-4">
          <span className="text-sm text-gray-600 dark:text-gray-300">{t("设备掉网（未注册蜂窝网络）")}</span>
          <Switch checked={value.network_lost} disabled={off} onChange={(network_lost) => update({ network_lost })} />
        </div>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Field label={t("判定时长（秒）")} hint={t("状态持续超过该时长才发送提醒，避免瞬时抖动。")}>
            <Input
              value={String(value.offline_grace_seconds)}
              onChange={(e) => update({ offline_grace_seconds: Number(e.target.value) || 0 })}
              disabled={off}
              type="number"
              inputMode="numeric"
            />
          </Field>
          <Field label={t("冷却时间（秒）")} hint={t("同一事件在冷却期内不会重复推送。")}>
            <Input
              value={String(value.cooldown_seconds)}
              onChange={(e) => update({ cooldown_seconds: Number(e.target.value) || 0 })}
              disabled={off}
              type="number"
              inputMode="numeric"
            />
          </Field>
        </div>
        <p className="text-xs leading-5 text-gray-500 dark:text-gray-400">
          {t("提醒会复用通知页中已启用的渠道（Telegram/Bark/Email/Pushplus/Gotify/Webhook/企业微信/飞书），请先至少启用一个渠道。")}
        </p>
        <div className="flex justify-end">
          <Button variant="primary" loading={saving} disabled={loading} onClick={save}>
            {t("保存设备通知配置")}
          </Button>
        </div>
      </div>
    </div>
  );
}
