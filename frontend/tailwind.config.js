/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        primary: { DEFAULT: '#6366f1', 50: '#eef2ff', 600: '#4f46e5', 700: '#4338ca' },
        claude: '#d97706',
        openai: '#10a37f',
        gemini: '#4285f4',
      },
    },
  },
  plugins: [],
}
