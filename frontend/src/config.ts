// Environment configuration for frontend
const getApiUrl = () => {
  if ((import.meta as any).env?.VITE_API_URL) {
    return (import.meta as any).env.VITE_API_URL
  }
  if (typeof window !== 'undefined' && window.location.origin) {
    return window.location.origin
  }
  return 'http://localhost:8080'
}

const config = {
  apiUrl: getApiUrl(),
  environment: (import.meta as any).env?.MODE || 'development',
  version: '1.0.0'
}

export default config
