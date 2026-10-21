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

// platformOptions 统一后台标准平台的规范键、名称和显示顺序，供筛选与公告目标共用。
export const platformOptions = [
  { key: "windows", label: "Windows" },
  { key: "macos", label: "macOS" },
  { key: "linux", label: "Linux" },
  { key: "android", label: "Android" },
  { key: "ios", label: "iOS" },
  { key: "harmony", label: "HarmonyOS" },
] as const;

// platformKey 统一把客户端上报的平台名转换为后台筛选使用的规范键；未知平台保留小写名称。
export function platformKey(platform: string): string {
  const key = platform.trim().toLowerCase();
  return Object.hasOwn(platformAliases, key) ? platformAliases[key] : key;
}

// platformName 统一后台各页面对客户端平台名称的显示，并保留未知值供排查。
export function platformName(platform: string): string {
  const key = platformKey(platform);
  return platformOptions.find(option => option.key === key)?.label ?? platform;
}
