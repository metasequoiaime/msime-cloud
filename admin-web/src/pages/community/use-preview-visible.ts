import { useEffect, useRef, useState } from "react";

// 卡片进入视口附近再读取图片和完整设计，避免整页同时下载照片。
export function usePreviewVisible(immediate = false) {
  const container = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(immediate);
  useEffect(() => {
    if (visible || !container.current) return;
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) { setVisible(true); observer.disconnect(); }
    }, { rootMargin: "200px" });
    observer.observe(container.current);
    return () => observer.disconnect();
  }, [visible]);
  return { container, visible };
}
