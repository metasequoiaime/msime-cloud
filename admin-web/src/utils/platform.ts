const platformNames: Record<string, string> = {
  windows: "Windows",
  win: "Windows",
  win32: "Windows",
  win64: "Windows",
  macos: "macOS",
  mac: "macOS",
  osx: "macOS",
  darwin: "macOS",
  linux: "Linux",
  android: "Android",
  ios: "iOS",
  iphone: "iOS",
  ipados: "iOS",
  harmonyos: "HarmonyOS",
  harmony: "HarmonyOS",
  ohos: "HarmonyOS",
  openharmony: "HarmonyOS",
};

// platformName 统一后台各页面对客户端平台名称的显示，并保留未知值供排查。
export function platformName(platform: string): string {
  return platformNames[platform.trim().toLowerCase()] ?? platform;
}
