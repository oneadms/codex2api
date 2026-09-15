import { useId } from 'react'

export type StudioScene = 'landscape' | 'product' | 'architecture'

// Decorative, local vector artwork for the studio's inspiration cards.
export function StudioArtwork({ scene, className }: { scene: StudioScene; className?: string }) {
  const id = useId()
  return (
    <svg viewBox="0 0 240 280" fill="none" className={className} aria-hidden="true" focusable="false">
      <defs>
        <linearGradient id={`${id}-sky`} x1="120" y1="0" x2="120" y2="280" gradientUnits="userSpaceOnUse">
          <stop stopColor={scene === 'landscape' ? '#BDCFCA' : scene === 'product' ? '#ECE7DC' : '#EACCB3'} />
          <stop offset="1" stopColor={scene === 'landscape' ? '#F3E9CD' : scene === 'product' ? '#C9D1C2' : '#F6E8D6'} />
        </linearGradient>
        <linearGradient id={`${id}-object`} x1="85" y1="100" x2="170" y2="215" gradientUnits="userSpaceOnUse">
          <stop stopColor="#647C60" />
          <stop offset="0.5" stopColor="#A4AE83" />
          <stop offset="1" stopColor="#536A51" />
        </linearGradient>
        <linearGradient id={`${id}-arch`} x1="90" y1="90" x2="170" y2="220" gradientUnits="userSpaceOnUse">
          <stop stopColor="#705E55" />
          <stop offset="1" stopColor="#CDAE90" />
        </linearGradient>
      </defs>
      <path fill={`url(#${id}-sky)`} d="M0 0h240v280H0z" />
      {scene === 'landscape' ? (
        <>
          <circle cx="171" cy="75" r="29" fill="#F8F1D8" />
          <path d="M0 174 66 93 153 200 196 156 240 192v88H0Z" fill="#82988C" />
          <path d="m28 137 38-44 40 49-26-13-11 8-14-17Z" fill="#DCE1CE" />
          <path d="M0 203c41-35 75-46 123-21s62 17 117-22v120H0Z" fill="#536D61" />
          <path d="M0 218c66-13 92 12 145 3s70-36 95-29v88H0Z" fill="#314E44" />
          <path d="M133 188c-52 23 56 35 22 48s-70 11-61 44h45c-30-22 41-27 40-43s-74-25-46-49Z" fill="#CBDBCB" />
          <path d="M20 45h39M20 51h21" stroke="#536D61" strokeOpacity=".4" />
        </>
      ) : scene === 'product' ? (
        <>
          <path d="M164 0h76v280H0v-29Z" fill="#FFF9EA" opacity=".5" />
          <ellipse cx="137" cy="226" rx="73" ry="11" fill="#718067" opacity=".15" />
          <path d="M43 227h151v53H43z" fill="#E9E6D9" />
          <path d="M43 227h151l-26-17H62z" fill="#F7F3E9" />
          <path d="M93 123h54l14 30v45c0 14-15 23-41 23s-41-9-41-23v-45Z" fill={`url(#${id}-object)`} />
          <ellipse cx="120" cy="123" rx="27" ry="7" fill="#51644D" />
          <ellipse cx="120" cy="123" rx="21" ry="4" fill="#303E32" />
          <path d="M119 125c3-29 0-53 14-90M123 87l-26-21M125 73l28-19M119 110l-32-17" stroke="#586F4D" strokeWidth="2" />
          <path d="M132 47c-11-20-3-30 5-31 6 13 5 21-5 31ZM128 73c6-21 22-30 32-25-3 15-14 26-32 25ZM114 83C91 83 82 72 84 60c15-2 25 8 30 23ZM119 110c-23 1-39-9-38-21 16-2 30 5 38 21Z" fill="#728866" />
          <path d="M92 151v42c0 9 5 14 12 16" stroke="#DEE0B9" strokeWidth="3" strokeLinecap="round" opacity=".35" />
        </>
      ) : (
        <>
          <path d="M0 0h47v280H0z" fill="#D3B598" />
          <path d="M47 0h193v230H47z" fill="#E9D3BA" />
          <path d="M81 222V108a48 48 0 0 1 96 0v114Z" fill="#F7E6CD" />
          <path d="M96 222V111a36 36 0 0 1 72 0v111Z" fill={`url(#${id}-arch)`} />
          <path d="M96 222V111a36 36 0 0 1 36-36v147Z" fill="#514E44" opacity=".45" />
          <path d="m177 222 63 42v16H70l26-58Z" fill="#FAEFDC" />
          <path d="M96 222h73l-13 10H86z" fill="#C4AB90" />
          <path d="M86 232h70v7H86z" fill="#A78F79" />
          <path d="M0 50 47 83v95L0 148Z" fill="#F7E6CD" opacity=".75" />
          <circle cx="201" cy="67" r="15" stroke="#B3977A" strokeWidth=".7" />
          <path d="M193 67h16M201 59v16" stroke="#B3977A" strokeWidth=".7" />
        </>
      )}
    </svg>
  )
}
