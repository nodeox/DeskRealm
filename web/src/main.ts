import { createApp, h, ref } from 'vue'
import App from './App.vue'
import './styles.css'

function readTicket() {
  if (!['/initialize', '/accept-invitation', '/recover'].includes(location.pathname) || !location.hash) return ''
  const value = new URLSearchParams(location.hash.slice(1)).get('ticket') ?? ''
  history.replaceState(null, '', location.pathname + location.search)
  return value
}

const ticket = ref(readTicket())
window.addEventListener('hashchange', () => { ticket.value = readTicket() })

createApp({ setup: () => () => h(App, { ticket: ticket.value }) }).mount('#app')
