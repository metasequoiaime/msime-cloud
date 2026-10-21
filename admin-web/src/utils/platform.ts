const platformAliases: Record<string, string> = {
  win: "windows",
  win32: "windows",
  win64: "windows",
  mac: "macos",
  osx: "macos",
  darwin: "macos",
  harmonyos: "harmony",
  ohos: "harmony",
  openharmony: "harmony",
  iphone: "ios",
  ipados: "ios",
};

const platformNames: Record<string, string> = {
  windows: "Windows",
  macos: "macOS",
  linux: "Linux",
  android: "Android",
  ios: "iOS",
  harmony: "HarmonyOS",
};

// platformKey 统一把客户端上报的平台名转换为后台筛选使用的规范键；未知平台保留小写名称。
export function platformKey(platform: string): string {
  const key = platform.trim().toLowerCase();
  return Object.hasOwn(platformAliases, key) ? platformAliases[key] : key;
}

// platformName 统一后台各页面对客户端平台名称的显示，并保留未知值供排查。
export function platformName(platform: string): string {
  const key = platformKey(platform);
  return Object.hasOwn(platformNames, key) ? platformNames[key] : platform;
}
