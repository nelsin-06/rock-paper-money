import { createClient } from '@supabase/supabase-js'

const url = import.meta.env.VITE_SUPABASE_URL
const publishableKey = import.meta.env.VITE_SUPABASE_PUBLISHABLE_KEY

export const supabase = url && publishableKey ? createClient(url, publishableKey) : null

export async function getAccessToken() {
  if (!supabase) throw new Error('Supabase authentication is not configured.')
  const { data, error } = await supabase.auth.getSession()
  if (error) throw error
  if (!data.session?.access_token) throw new Error('Your account session has expired. Sign in again.')
  return data.session.access_token
}
