import { useContext } from "react";
import { useQuery } from "@tanstack/react-query";
import type { QueryKey } from "@tanstack/react-query";
import { APICredentialsContext, APIError } from "../api/client";

type APIImageOptions = { queryKey: QueryKey; path: string; enabled?: boolean };

// 管理接口的图片使用 Cookie 或旧令牌读取，再转为后台 CSP 允许的 data: URL。
export function useAPIImage({ queryKey, path, enabled = true }: APIImageOptions) {
  const credentials = useContext(APICredentialsContext);
  const token = credentials?.token;
  return useQuery({
    queryKey,
    enabled,
    staleTime: 5 * 60_000,
    queryFn: async ({ signal }) => {
      const response = await fetch(path, { signal, credentials: "same-origin", headers: token ? { Authorization: `Bearer ${token}` } : {} });
      if (!response.ok) throw new APIError(response.status, "");
      const blob = await response.blob();
      return await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(blob);
      });
    },
  });
}
