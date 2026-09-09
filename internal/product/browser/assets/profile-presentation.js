// Shared, literal-text profile presentation for library and selection surfaces.
(()=>{
 const availability=profile=>{
  const disabled=profile.Disabled??profile.disabled??false;
  const reason=profile.DisabledReason??profile.disabled_reason??'';
  return (disabled?'Disabled':'Enabled')+(reason?' · '+reason:'');
 };
 const label=(profile,includeEnabled=false)=>(profile.Name??profile.name??'')+((includeEnabled||(profile.Disabled??profile.disabled)||(profile.DisabledReason??profile.disabled_reason))?' · '+availability(profile):'');
 globalThis.ProfilePresentation=Object.freeze({availability,label});
})();
