// Snapshot clipboard files during the paste event; the transfer becomes
// inaccessible afterwards. Prefer FileList so mirrored items aren't uploaded twice.
export function filesFromClipboard(data: DataTransfer): File[] {
  const files = Array.from(data.files);
  if (files.length > 0) return files;
  return Array.from(data.items).flatMap((item) => {
    const file = item.kind === "file" ? item.getAsFile() : null;
    return file ? [file] : [];
  });
}
