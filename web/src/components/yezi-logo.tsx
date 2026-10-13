import type { SVGProps } from "react";

import { cn } from "@/lib/utils";

/**
 * Yezi 叶子标识。
 *
 * 字形取自 Lucide 的 leaf 图标（ISC License，Copyright (c) 2026 Lucide Icons and Contributors），
 * 原始路径：https://github.com/lucide-icons/lucide/blob/main/icons/leaf.svg
 * 许可全文与改动说明见 web/THIRD_PARTY_NOTICES.md。
 *
 * 描边使用 currentColor，随所在位置的前景色变化，在深浅背景下都保持对比。
 */
export function YeziLogo({ className, ...props }: SVGProps<SVGSVGElement>) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      role="img"
      aria-label="Yezi"
      className={className}
      {...props}
    >
      <path d="M11 20a10 10 0 0010-10 25.9 25.9 0 00-1.04-7.281 1 1 0 00-1.755-.325C15.833 5.5 13 5.5 9.8 6.1A7 7 0 0011 20" />
      <path d="M2 21a5 5 0 012.911-4.544C7.613 15.212 8.351 15.24 11 13" />
    </svg>
  );
}

/**
 * 侧栏等小尺寸场景使用：品牌绿底 + 白色叶子，与 favicon（src/app/icon.svg）一致，
 * 浅色/深色侧栏下都清晰可辨。
 */
export function YeziLogoBadge({ className }: { className?: string }) {
  return (
    <span
      className={cn("flex size-8 shrink-0 items-center justify-center rounded-lg bg-emerald-600 text-white", className)}
    >
      {/* 侧栏按钮对后代 svg 有 [&_svg]:size-4 约束，这里用 ! 保证叶子占满徽标。 */}
      <YeziLogo className="size-5!" strokeWidth={2.25} />
    </span>
  );
}
