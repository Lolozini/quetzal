// The Quetzal lockup: the Quetzalcoatlus and the name, from docs/brand. These are
// the dark-ground files (a hairline white outline around the bird, the name in
// cream), the panel's only theme.
export function Lockup({ stacked = false }: { stacked?: boolean }) {
  return stacked ? (
    <img className="lockup lockup-stacked" src="/brand/quetzal-lockup-stacked-dark.svg" alt="Quetzal" width={168} height={150} />
  ) : (
    <img className="lockup" src="/brand/quetzal-lockup-dark.svg" alt="Quetzal" width={162} height={40} />
  );
}
