export function readBlobDataURL(
  blob: Blob,
  onLoad: (value: string) => void,
  onError: (error: DOMException | null) => void,
): () => void {
  const reader = new FileReader();
  reader.onload = () => onLoad(String(reader.result));
  reader.onerror = () => onError(reader.error);
  reader.readAsDataURL(blob);
  return () => {
    reader.onload = null;
    reader.onerror = null;
    if (reader.readyState === FileReader.LOADING) reader.abort();
  };
}
