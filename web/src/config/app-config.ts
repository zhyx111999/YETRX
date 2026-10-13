import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "YETRX",
  version: packageJson.version,
  // 保留上游 ARTEX 出处（AGPL-3.0 要求保留原作者版权与来源声明）。
  copyright: `© ${currentYear}, YETRX. Based on ARTEX.`,
  meta: {
    title: "YETRX-自主维护",
    description: "YETRX 自主维护控制台：LLM 驱动的自动化运维，覆盖任务编排、巡检、处置与告警通知",
  },
};
