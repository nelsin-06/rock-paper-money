export function backendURL(path) {
  const target = import.meta.env.VITE_BACKEND_PROXY_TARGET
  return target ? new URL(path, target).toString() : path
}
